#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
ACTIVATE="$ROOT/proxypool-core/files/proxypool-backend-activate"
ACTIVATE_INIT="$ROOT/proxypool-core/files/proxypool-activate.init"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

fail() {
	printf '%s\n' "$*" >&2
	exit 1
}

[ -f "$ACTIVATE" ] || fail 'missing backend activation helper'
[ -f "$ACTIVATE_INIT" ] || fail 'missing backend activation init script'

BIN="$TEST_TMP/bin"
mkdir -p "$BIN"

GO_BIN=${PROXYPOOL_TEST_GO:-}
if [ -z "$GO_BIN" ]; then
	GO_BIN=$(command -v go) || fail 'Go toolchain is required for the real selector parser gate'
fi
REAL_CTL="$BIN/proxypoolctl-real"
(
	cd "$ROOT/proxypool-core/src/proxypoold"
	"$GO_BIN" build -o "$REAL_CTL" ./cmd/proxypoolctl
)

cat >"$BIN/flock" <<'EOF'
#!/usr/bin/env sh
[ "$#" -eq 2 ] && [ "$1" = -x ] && [ "$2" = 9 ]
EOF

cat >"$BIN/sync" <<'EOF'
#!/usr/bin/env sh
exit 0
EOF

cat >"$BIN/id" <<'EOF'
#!/usr/bin/env sh
[ "$#" -eq 1 ] && [ "$1" = -u ] || exit 2
printf '%s\n' 0
EOF

cat >"$BIN/proxypoolctl" <<'EOF'
#!/usr/bin/env sh
case "${1-}" in
	select-backend|classify)
		exec "$PROXYPOOL_TEST_REAL_CTL" "$@"
		;;
	procd-state)
		[ "$#" -eq 3 ] && [ "$2" = --service ] && [ "$3" = proxypool ] || exit 2
		cat "$PROXYPOOL_TEST_PROCD_STATE_FILE"
		;;
	*) exit 2 ;;
esac
EOF

cat >"$BIN/migrate" <<'EOF'
#!/usr/bin/env sh
[ "$#" -eq 0 ] || exit 2
printf '%s\n' migrate >>"$PROXYPOOL_TEST_MIGRATE_LOG"
printf '%s\n' "$PROXYPOOL_LEGACY_CONFIG" "$PROXYPOOL_V2_CONFIG" "$PROXYPOOL_STATE_DIR" >>"$PROXYPOOL_TEST_MIGRATE_LOG"
[ "${PROXYPOOL_TEST_MIGRATE_FAIL:-0}" = 0 ]
EOF

chmod 755 "$BIN/flock" "$BIN/sync" "$BIN/id" "$BIN/proxypoolctl" "$BIN/migrate"

CASE_ROOT=
reset_case() {
	name=$1
	CASE_ROOT="$TEST_TMP/$name"
	rm -rf "$CASE_ROOT"
	mkdir -p \
		"$CASE_ROOT/etc/config" \
		"$CASE_ROOT/etc/proxypool" \
		"$CASE_ROOT/usr/lib/proxypool" \
		"$CASE_ROOT/run" \
		"$CASE_ROOT/lock"
	cp "$ROOT/files/etc/config/proxypool" "$CASE_ROOT/etc/config/proxypool"
	cp "$ROOT/files/etc/config/proxypool_v2" "$CASE_ROOT/etc/config/proxypool_v2"
	printf '%s\n' \
		"config global 'global'" \
		"	option runtime_backend 'v1'" >"$CASE_ROOT/etc/config/proxypool_runtime"
	printf '%s\n' '11111111-1111-4111-8111-111111111111' >"$CASE_ROOT/boot-id"
	printf '%s\n' absent >"$CASE_ROOT/procd-state"
	: >"$CASE_ROOT/migrate.log"
	chmod 700 "$CASE_ROOT/etc/proxypool"
}

run_activate() {
	PROXYPOOL_CTL="$BIN/proxypoolctl" \
	PROXYPOOL_TEST_REAL_CTL="$REAL_CTL" \
	PROXYPOOL_MIGRATE="$BIN/migrate" \
	PROXYPOOL_FLOCK="$BIN/flock" \
	PROXYPOOL_SYNC="$BIN/sync" \
	PROXYPOOL_ID="$BIN/id" \
	PROXYPOOL_LEGACY_CONFIG="$CASE_ROOT/etc/config/proxypool" \
	PROXYPOOL_V2_CONFIG="$CASE_ROOT/etc/config/proxypool_v2" \
	PROXYPOOL_SELECTOR_FILE="$CASE_ROOT/etc/config/proxypool_runtime" \
	PROXYPOOL_STATE_DIR="$CASE_ROOT/etc/proxypool" \
	PROXYPOOL_ACTIVATED_BACKEND="$CASE_ROOT/etc/proxypool/activated-backend" \
	PROXYPOOL_CLEANUP_REQUIRED="$CASE_ROOT/etc/proxypool/cleanup-required" \
	PROXYPOOL_ACTIVATION_REQUEST="$CASE_ROOT/etc/proxypool/v2-activation-request" \
	PROXYPOOL_IMAGE_AUTHORIZATION="$CASE_ROOT/usr/lib/proxypool/v2-image-activation-authority" \
	PROXYPOOL_RUNTIME_MARKER="$CASE_ROOT/run/proxypool.backend" \
	PROXYPOOL_QUARANTINE_MARKER="$CASE_ROOT/run/proxypool.backend.cleanup-required" \
	PROXYPOOL_TRANSITION_MARKER="$CASE_ROOT/run/proxypool.transition" \
	PROXYPOOL_ACTIVE_SNAPSHOT_MARKER="$CASE_ROOT/run/proxypool.config-snapshot" \
	PROXYPOOL_LEGACY_RUN_DIR="$CASE_ROOT/run/proxypool" \
	PROXYPOOL_SOCKET="$CASE_ROOT/run/proxypoold.sock" \
	PROXYPOOL_BOOT_ID_FILE="$CASE_ROOT/boot-id" \
	PROXYPOOL_ACTIVATION_LOCK="$CASE_ROOT/lock/backend-activation.lock" \
	PROXYPOOL_TEST_PROCD_STATE_FILE="$CASE_ROOT/procd-state" \
	PROXYPOOL_TEST_MIGRATE_LOG="$CASE_ROOT/migrate.log" \
		sh "$ACTIVATE"
}

assert_v2_committed() {
	printf '%s\n' \
		"config global 'global'" \
		"	option runtime_backend 'v2_shadow'" >"$CASE_ROOT/expected-selector"
	cmp -s "$CASE_ROOT/expected-selector" "$CASE_ROOT/etc/config/proxypool_runtime" ||
		fail 'activation did not publish the exact V2 selector'
	[ "$(cat "$CASE_ROOT/etc/proxypool/activated-backend")" = v2_shadow ] ||
		fail 'activation did not publish V2 ownership'
	[ ! -e "$CASE_ROOT/etc/proxypool/cleanup-required" ] ||
		fail 'activation retained stale V1 cleanup evidence'
	[ ! -e "$CASE_ROOT/etc/proxypool/v2-activation-request" ] ||
		fail 'activation retained a completed request'
}

set_selector_class() {
	class=$1
	case "$class" in
		missing)
			rm -f "$CASE_ROOT/etc/config/proxypool_runtime"
			;;
		v1|v2_shadow)
			printf '%s\n' \
				"config global 'global'" \
				"	option runtime_backend '$class'" >"$CASE_ROOT/etc/config/proxypool_runtime"
			;;
		unknown)
			printf '%s\n' \
				"config global 'global'" \
				"	option enabled '1'" \
				"	option max_clients '60'" >"$CASE_ROOT/etc/config/proxypool_runtime"
			;;
		*) fail "invalid test selector class: $class" ;;
	esac
}

set_backend_marker() {
	path=$1
	value=$2
	case "$value" in
		empty) rm -f "$path" ;;
		v1|v2_shadow) printf '%s\n' "$value" >"$path" ;;
		*) fail "invalid test backend marker: $value" ;;
	esac
}

migration_count() {
	count=$(grep -c '^migrate$' "$CASE_ROOT/migrate.log" 2>/dev/null) || count=0
	printf '%s' "$count"
}

run_image_recovery_case() {
	name=$1
	selector_class=$2
	owner=$3
	cleanup=$4
	expected_migrations=$5
	reset_case "$name"
	set_selector_class "$selector_class"
	set_backend_marker "$CASE_ROOT/etc/proxypool/activated-backend" "$owner"
	set_backend_marker "$CASE_ROOT/etc/proxypool/cleanup-required" "$cleanup"
	printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
	run_activate || fail "$name did not converge"
	assert_v2_committed
	[ "$(migration_count)" -eq "$expected_migrations" ] ||
		fail "$name used the wrong migration lineage"
}

assert_rejected_without_persistent_mutation() {
	name=$1
	snapshot="$CASE_ROOT/persistent-before"
	cp -a "$CASE_ROOT/etc" "$snapshot"
	if run_activate >/dev/null 2>&1; then
		fail "$name unexpectedly converged"
	fi
	diff -r "$snapshot" "$CASE_ROOT/etc" >/dev/null ||
		fail "$name changed persistent state before rejection"
	[ "$(migration_count)" -eq 0 ] || fail "$name ran migration before rejection"
}

run_rejected_tuple() {
	name=$1
	selector_class=$2
	owner=$3
	cleanup=$4
	reset_case "$name"
	set_selector_class "$selector_class"
	set_backend_marker "$CASE_ROOT/etc/proxypool/activated-backend" "$owner"
	set_backend_marker "$CASE_ROOT/etc/proxypool/cleanup-required" "$cleanup"
	printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
	assert_rejected_without_persistent_mutation "$name"
}

# A sysupgrade keep archive can contain a pre-selector V1 file at the selector
# path.  The image request and the absence of backend ownership make this a
# cold legacy migration, provided the real legacy configuration still proves
# V1.  The stale compatibility file is control data, not user configuration.
reset_case preserved-pre-selector-v1
printf '%s\n' \
	"config global 'global'" \
	"	option enabled '1'" \
	"	option max_clients '60'" >"$CASE_ROOT/etc/config/proxypool_runtime"
printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
run_activate || fail 'cold image did not recover a preserved pre-selector V1 file'
assert_v2_committed
[ "$(grep -c '^migrate$' "$CASE_ROOT/migrate.log")" -eq 1 ] ||
	fail 'preserved pre-selector V1 recovery did not migrate exactly once'

reset_case preserved-invalid-package-request
printf '%s\n' \
	"config global 'global'" \
	"	option enabled '1'" \
	"	option max_clients '60'" >"$CASE_ROOT/etc/config/proxypool_runtime"
printf '%s\n' 'boot:22222222-2222-4222-8222-222222222222' >"$CASE_ROOT/etc/proxypool/v2-activation-request"
if run_activate >/dev/null 2>&1; then
	fail 'package request recovered an invalid preserved selector'
fi
[ ! -s "$CASE_ROOT/migrate.log" ] ||
	fail 'invalid selector package request ran migration'

# The release-10 field regression: an older image can preserve both a
# pre-selector V1 compatibility file and the exact V1 ownership reservation.
# A cold image boot with no runtime/procd evidence must reclaim that stopped V1
# ownership and migrate once instead of remaining permanently fail-closed.
reset_case preserved-invalid-v1-owned
printf '%s\n' \
	"config global 'global'" \
	"	option enabled '1'" \
	"	option max_clients '60'" >"$CASE_ROOT/etc/config/proxypool_runtime"
printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
printf '%s\n' v1 >"$CASE_ROOT/etc/proxypool/activated-backend"
run_activate || fail 'cold image did not recover a preserved V1 owner with an invalid selector'
assert_v2_committed
[ "$(grep -c '^migrate$' "$CASE_ROOT/migrate.log")" -eq 1 ] ||
	fail 'preserved invalid V1 owner recovery did not migrate exactly once'

# Complete safe cold-image matrix.  V1 lineage always migrates exactly once;
# a proven V2 owner repairs control state without ever re-running V1 migration.
run_image_recovery_case image-missing-unowned missing empty empty 1
run_image_recovery_case image-v1-unowned v1 empty empty 1
run_image_recovery_case image-v1-owned v1 v1 empty 1
run_image_recovery_case image-v1-owned-wal v1 v1 v1 1
run_image_recovery_case image-unknown-v1-wal unknown v1 v1 1
run_image_recovery_case image-missing-v2-owned missing v2_shadow empty 0
run_image_recovery_case image-v1-v2-owned v1 v2_shadow empty 0
run_image_recovery_case image-unknown-v2-owned unknown v2_shadow empty 0
run_image_recovery_case image-v2-v2-owned v2_shadow v2_shadow empty 0
run_image_recovery_case image-missing-v2-wal missing v2_shadow v2_shadow 0
run_image_recovery_case image-v1-v2-wal v1 v2_shadow v2_shadow 0
run_image_recovery_case image-unknown-v2-wal unknown v2_shadow v2_shadow 0
run_image_recovery_case image-v2-v2-wal v2_shadow v2_shadow v2_shadow 0

# Every cross-lineage or ownerless WAL tuple is terminal and must leave all
# persistent input byte-for-byte unchanged.
run_rejected_tuple reject-unknown-ownerless-v1-wal unknown empty v1
run_rejected_tuple reject-unknown-ownerless-v2-wal unknown empty v2_shadow
run_rejected_tuple reject-v1-owner-v2-wal unknown v1 v2_shadow
run_rejected_tuple reject-v2-owner-v1-wal unknown v2_shadow v1
run_rejected_tuple reject-v2-selector-unowned v2_shadow empty empty
run_rejected_tuple reject-v2-selector-v1-owner v2_shadow v1 empty

reset_case reject-invalid-v1-lineage-config
set_selector_class unknown
printf '%s\n' "config global 'global'" "	option enabled '0'" >"$CASE_ROOT/etc/config/proxypool"
printf '%s\n' v1 >"$CASE_ROOT/etc/proxypool/activated-backend"
printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
assert_rejected_without_persistent_mutation reject-invalid-v1-lineage-config

reset_case reject-invalid-v2-lineage-config
set_selector_class unknown
printf '%s\n' "config global 'global'" "	option schema_version '1'" >"$CASE_ROOT/etc/config/proxypool_v2"
printf '%s\n' v2_shadow >"$CASE_ROOT/etc/proxypool/activated-backend"
printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
assert_rejected_without_persistent_mutation reject-invalid-v2-lineage-config

# The field failure: release 7 selected V1, and a quarantined attempt could
# leave exact V1 ownership/WAL evidence.  A different boot ID proves that all
# legacy processes, interfaces, and kernel-only state crossed a reboot before
# the migration transaction reclaims that stopped ownership.
reset_case cold-v1-recovery
printf '%s\n' 'boot:22222222-2222-4222-8222-222222222222' >"$CASE_ROOT/etc/proxypool/v2-activation-request"
printf '%s\n' v1 >"$CASE_ROOT/etc/proxypool/activated-backend"
printf '%s\n' v1 >"$CASE_ROOT/etc/proxypool/cleanup-required"
run_activate || fail 'cold V1 recovery did not activate V2'
assert_v2_committed
[ "$(grep -c '^migrate$' "$CASE_ROOT/migrate.log")" -eq 1 ] ||
	fail 'cold V1 recovery did not migrate exactly once'

# Re-running after commit is a no-op, even without a request and regardless of
# whether procd now owns the V2 service.
printf '%s\n' present >"$CASE_ROOT/procd-state"
run_activate || fail 'completed V2 activation is not idempotent'
[ "$(grep -c '^migrate$' "$CASE_ROOT/migrate.log")" -eq 1 ] ||
	fail 'idempotent activation repeated migration'

# A live package upgrade may only queue activation.  It must not reinterpret
# process/PID absence in the current boot as proof that legacy network state is
# gone; the next boot is the safety boundary.
reset_case same-boot
printf '%s\n' 'boot:11111111-1111-4111-8111-111111111111' >"$CASE_ROOT/etc/proxypool/v2-activation-request"
if run_activate >/dev/null 2>&1; then
	fail 'same-boot package upgrade activated V2 without a reboot boundary'
fi
grep -Fq "runtime_backend 'v1'" "$CASE_ROOT/etc/config/proxypool_runtime" ||
	fail 'same-boot refusal changed the selector'
[ ! -e "$CASE_ROOT/etc/proxypool/activated-backend" ] ||
	fail 'same-boot refusal claimed backend ownership'
[ ! -s "$CASE_ROOT/migrate.log" ] || fail 'same-boot refusal ran migration'

# A settings-preserving sysupgrade can retain an overlay whiteout for the
# consumed one-time request.  The full image's separate authorization witness
# must recover a cold, stopped V2 owner without weakening package-install
# reboot gating.  This witness is staged only by the full firmware workflow.
reset_case image-request-whiteout
printf '%s\n' v2_shadow >"$CASE_ROOT/etc/proxypool/activated-backend"
printf '%s\n' v2-image-activation-v1 >"$CASE_ROOT/usr/lib/proxypool/v2-image-activation-authority"
run_activate || fail 'cold image authorization did not recover a whiteouted activation request'
assert_v2_committed
[ ! -s "$CASE_ROOT/migrate.log" ] || fail 'whiteout recovery re-ran V1 migration'

# The same stopped state without the full-image witness remains fail-closed;
# installing the core IPK alone must never grant cold image authority.
reset_case request-missing-without-image-authorization
printf '%s\n' v2_shadow >"$CASE_ROOT/etc/proxypool/activated-backend"
if run_activate >/dev/null 2>&1; then
	fail 'missing request activated without full-image authorization'
fi
[ ! -s "$CASE_ROOT/migrate.log" ] || fail 'unauthorized missing-request recovery ran migration'

# Even after a reboot request becomes eligible, any boot-local legacy or procd
# evidence keeps the router fail-closed and leaves the durable request intact.
reset_case legacy-evidence
printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
mkdir "$CASE_ROOT/run/proxypool"
if run_activate >/dev/null 2>&1; then
	fail 'activation ignored legacy runtime evidence'
fi
[ -f "$CASE_ROOT/etc/proxypool/v2-activation-request" ] ||
	fail 'legacy-evidence refusal discarded the retry request'
[ ! -s "$CASE_ROOT/migrate.log" ] || fail 'legacy-evidence refusal ran migration'

reset_case procd-evidence
printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
printf '%s\n' present >"$CASE_ROOT/procd-state"
assert_rejected_without_persistent_mutation procd-evidence

reset_case procd-unknown
printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
printf '%s\n' unknown >"$CASE_ROOT/procd-state"
assert_rejected_without_persistent_mutation procd-unknown

for evidence_name in runtime quarantine transition snapshot socket; do
	reset_case "boot-evidence-$evidence_name"
	printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
	case "$evidence_name" in
		runtime) evidence_path="$CASE_ROOT/run/proxypool.backend" ;;
		quarantine) evidence_path="$CASE_ROOT/run/proxypool.backend.cleanup-required" ;;
		transition) evidence_path="$CASE_ROOT/run/proxypool.transition" ;;
		snapshot) evidence_path="$CASE_ROOT/run/proxypool.config-snapshot" ;;
		socket) evidence_path="$CASE_ROOT/run/proxypoold.sock" ;;
	esac
	printf '%s\n' evidence >"$evidence_path"
	assert_rejected_without_persistent_mutation "boot-evidence-$evidence_name"
done

# If power is lost after V2 ownership is durable but before the selector is
# replaced, the preserved request makes that exact intermediate state
# recoverable on a later boot; all other cross-backend combinations fail.
reset_case interrupted-after-owner
printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
printf '%s\n' v2_shadow >"$CASE_ROOT/etc/proxypool/activated-backend"
run_activate || fail 'interrupted V2 ownership publication was not recoverable'
assert_v2_committed

reset_case invalid-cross-owner
printf '%s\n' image >"$CASE_ROOT/etc/proxypool/v2-activation-request"
printf '%s\n' v1 >"$CASE_ROOT/etc/proxypool/activated-backend"
printf '%s\n' \
	"config global 'global'" \
	"	option runtime_backend 'v2_shadow'" >"$CASE_ROOT/etc/config/proxypool_runtime"
if run_activate >/dev/null 2>&1; then
	fail 'activation accepted an impossible selector/owner order'
fi
[ ! -s "$CASE_ROOT/migrate.log" ] || fail 'invalid cross-owner state ran migration'

reset_case malformed-completed-request
printf '%s\n' v2_shadow >"$CASE_ROOT/etc/proxypool/activated-backend"
printf '%s\n' \
	"config global 'global'" \
	"	option runtime_backend 'v2_shadow'" >"$CASE_ROOT/etc/config/proxypool_runtime"
printf '%s\n' 'boot:not-a-boot-id' >"$CASE_ROOT/etc/proxypool/v2-activation-request"
if run_activate >/dev/null 2>&1; then
	fail 'completed V2 activation discarded a malformed request'
fi
[ -f "$CASE_ROOT/etc/proxypool/v2-activation-request" ] ||
	fail 'malformed completed request was removed'

cat >"$BIN/activation-fail" <<'EOF'
#!/usr/bin/env sh
exit 73
EOF
chmod 755 "$BIN/activation-fail"
if PROXYPOOL_BACKEND_ACTIVATOR="$BIN/activation-fail" sh -c '. "$1"; start' sh "$ACTIVATE_INIT" >/dev/null 2>&1; then
	fail 'S98 activation init swallowed a backend activation failure'
else
	status=$?
fi
[ "$status" -eq 73 ] || fail 'S98 activation init changed the backend activation status'

echo 'backend cold activation integration: PASS'
