#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
POSTINST="$ROOT/proxypool-core/files/proxypool-postinst"
COLD_WRAPPER="$ROOT/proxypool-core/files/proxypool-safety-uci-default"
MAIN_INIT="$ROOT/proxypool-core/files/proxypool.init"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

[ -f "$POSTINST" ] || {
	echo 'missing package post-install helper' >&2
	exit 1
}
[ -f "$COLD_WRAPPER" ] || {
	echo 'missing cold-boot uci-default wrapper template' >&2
	exit 1
}
[ -f "$MAIN_INIT" ] || {
	echo 'missing main ProxyPool init script' >&2
	exit 1
}

SYNC="$TEST_TMP/sync"
printf '%s\n' '#!/usr/bin/env sh' 'exit 0' >"$SYNC"
chmod 755 "$SYNC"

make_probe() {
	path=$1
	label=$2
	status=$3
	printf '%s\n' \
		'#!/usr/bin/env sh' \
		"printf '%s:%s\\n' '$label' \"\${PROXYPOOL_COLD_BOOT:-unset}\" >>'$TEST_TMP/events'" \
		"exit $status" >"$path"
	chmod 755 "$path"
}

# ImageBuilder/root-prefix installation must only publish the late cold
# wrapper.  Executing either target init/defaults helper here is a host-side
# build bug and is recorded by the probes.
IMAGE_ROOT="$TEST_TMP/image-root"
mkdir -p "$IMAGE_ROOT/usr/lib/proxypool" "$IMAGE_ROOT/etc/init.d" "$IMAGE_ROOT/etc/uci-defaults"
cp "$COLD_WRAPPER" "$IMAGE_ROOT/usr/lib/proxypool/proxypool-safety-uci-default"
make_probe "$IMAGE_ROOT/usr/lib/proxypool/proxypool-firewall-defaults" image-defaults 99
make_probe "$IMAGE_ROOT/etc/init.d/proxypool-guard" image-guard 99
: >"$TEST_TMP/events"
IPKG_INSTROOT="$IMAGE_ROOT" PROXYPOOL_SYNC="$SYNC" sh "$POSTINST"
IMAGE_SCHEDULED="$IMAGE_ROOT/etc/uci-defaults/99-proxypool-safety"
[ -f "$IMAGE_SCHEDULED" ] && [ ! -L "$IMAGE_SCHEDULED" ] || {
	echo 'root-prefix post-install did not schedule the cold wrapper' >&2
	exit 1
}
cmp -s "$COLD_WRAPPER" "$IMAGE_SCHEDULED" || {
	echo 'root-prefix post-install changed the cold wrapper content' >&2
	exit 1
}
[ "$(stat -c '%a' "$IMAGE_SCHEDULED")" = 755 ] || {
	echo 'root-prefix post-install used the wrong wrapper mode' >&2
	exit 1
}
[ ! -s "$TEST_TMP/events" ] || {
	echo 'root-prefix post-install executed a target helper' >&2
	exit 1
}

# OpenWrt 23.05 default_postinst continues by starting every init shipped in
# the IPK even when custom postinst only queued a cold fallback.  The main init
# itself must exit before classifier/procd/backend mutation until S19 has
# published the runtime-bound activation marker.
PENDING_TEMPLATE="$TEST_TMP/pending-safety-template"
PENDING_HELPER="$TEST_TMP/pending-transaction"
PENDING_LAN_HELPER="$TEST_TMP/pending-lan-isolation"
PENDING_LS="$TEST_TMP/pending-ls"
PENDING_DEFERRED_START="$TEST_TMP/pending-start-deferred"
cp "$COLD_WRAPPER" "$PENDING_TEMPLATE"
chmod 755 "$PENDING_TEMPLATE"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	'[ "$#" -eq 1 ] || exit 2' \
	"printf '%s\n' \"\$1\" >>'$TEST_TMP/events'" \
	'case "$1" in' \
	'  journal-present) [ "${PROXYPOOL_TEST_JOURNAL_PRESENT:-0}" = 1 ] ;;' \
	'  activation-runtime-current) exit 1 ;;' \
	'  *) exit 2 ;;' \
	'esac' >"$PENDING_HELPER"
chmod 755 "$PENDING_HELPER"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	"printf '%s\\n' unexpected-lan-apply >>'$TEST_TMP/events'" \
	'exit 99' >"$PENDING_LAN_HELPER"
chmod 755 "$PENDING_LAN_HELPER"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	'[ "$#" -eq 2 ] && [ "$1" = -nd ] || exit 2' \
	'printf "%s\\n" "-rwxr-xr-x 1 0 0 0 Jan 1 00:00 $2"' >"$PENDING_LS"
chmod 755 "$PENDING_LS"
: >"$TEST_TMP/events"
PROXYPOOL_FIREWALL_SAFETY_TEMPLATE="$PENDING_TEMPLATE" \
	PROXYPOOL_FIREWALL_TRANSACTION_HELPER="$PENDING_HELPER" \
	PROXYPOOL_LAN_ISOLATION="$PENDING_LAN_HELPER" \
	PROXYPOOL_LS_PROG="$PENDING_LS" \
	PROXYPOOL_DEFERRED_START_MARKER="$PENDING_DEFERRED_START" \
	sh -c '. "$1"; start_service; printf "%s\n" unsafe-after-gate >>"$2"' \
		sh "$MAIN_INIT" "$TEST_TMP/events" || {
	echo 'main init returned failure instead of safely deferring default_postinst start' >&2
	exit 1
}
printf '%s\n' journal-present activation-current >"$TEST_TMP/expected-events"
cmp -s "$TEST_TMP/expected-events" "$TEST_TMP/events" || {
	echo 'main init crossed the firewall activation gate during default_postinst start' >&2
	echo 'actual events:' >&2
	cat "$TEST_TMP/events" >&2
	exit 1
}
[ -f "$PENDING_DEFERRED_START" ] && [ ! -L "$PENDING_DEFERRED_START" ] &&
	[ "$(cat "$PENDING_DEFERRED_START")" = pending ] || {
	echo 'deferred default_postinst start did not publish an isolated retry request' >&2
	exit 1
}

# Even a still-current old hash marker must not outrank an in-flight durable
# transaction.  The init gate must stop after journal detection and never ask
# the activation predicate (or reach backend/procd code).
: >"$TEST_TMP/events"
PROXYPOOL_TEST_JOURNAL_PRESENT=1 \
	PROXYPOOL_FIREWALL_SAFETY_TEMPLATE="$PENDING_TEMPLATE" \
	PROXYPOOL_FIREWALL_TRANSACTION_HELPER="$PENDING_HELPER" \
	PROXYPOOL_LAN_ISOLATION="$PENDING_LAN_HELPER" \
	PROXYPOOL_LS_PROG="$PENDING_LS" \
	PROXYPOOL_DEFERRED_START_MARKER="$PENDING_DEFERRED_START" \
	sh -c '. "$1"; start_service; printf "%s\n" unsafe-after-gate >>"$2"' \
		sh "$MAIN_INIT" "$TEST_TMP/events" || {
	echo 'main init returned failure instead of deferring a journal-pending start' >&2
	exit 1
}
printf '%s\n' journal-present >"$TEST_TMP/expected-events"
cmp -s "$TEST_TMP/expected-events" "$TEST_TMP/events" || {
	echo 'main init consulted stale activation authority while a journal existed' >&2
	exit 1
}

# A successful live conversion explicitly enables S18, runs in live mode, and
# retires any queued reboot fallback.
LIVE_DIR="$TEST_TMP/live"
mkdir -p "$LIVE_DIR/uci-defaults"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	'[ "$#" -eq 1 ] && [ "$1" = enable ] || exit 97' \
	"printf '%s:%s\\n' 'guard-enable' \"\${PROXYPOOL_COLD_BOOT:-unset}\" >>'$TEST_TMP/events'" \
	'exit 0' >"$LIVE_DIR/guard"
chmod 755 "$LIVE_DIR/guard"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	'[ "$#" -eq 1 ] && [ "$1" = request ] || exit 96' \
	"printf '%s\\n' 'lan-request' >>'$TEST_TMP/events'" \
	'exit 0' >"$LIVE_DIR/lan-isolation"
chmod 755 "$LIVE_DIR/lan-isolation"
make_probe "$LIVE_DIR/defaults-ok" defaults 0
printf '%s\n' stale >"$LIVE_DIR/uci-defaults/99-proxypool-safety"
: >"$TEST_TMP/events"
PROXYPOOL_GUARD_INIT="$LIVE_DIR/guard" \
	PROXYPOOL_LAN_ISOLATION="$LIVE_DIR/lan-isolation" \
	PROXYPOOL_FIREWALL_DEFAULTS="$LIVE_DIR/defaults-ok" \
	PROXYPOOL_COLD_WRAPPER_TEMPLATE="$COLD_WRAPPER" \
	PROXYPOOL_UCI_DEFAULTS_DIR="$LIVE_DIR/uci-defaults" \
	PROXYPOOL_SYNC="$SYNC" \
	sh "$POSTINST"
printf '%s\n' 'lan-request' 'guard-enable:unset' 'defaults:0' >"$TEST_TMP/expected-events"
cmp -s "$TEST_TMP/expected-events" "$TEST_TMP/events" || {
	echo 'live post-install did not enable guardian before live defaults' >&2
	exit 1
}
[ ! -e "$LIVE_DIR/uci-defaults/99-proxypool-safety" ] || {
	echo 'successful live post-install retained a stale cold wrapper' >&2
	exit 1
}

# A normal live safety-gate rejection must not wedge opkg.  It leaves an exact
# executable cold fallback for the next reboot.
make_probe "$LIVE_DIR/defaults-fail" defaults-fail 1
: >"$TEST_TMP/events"
PROXYPOOL_GUARD_INIT="$LIVE_DIR/guard" \
PROXYPOOL_LAN_ISOLATION="$LIVE_DIR/lan-isolation" \
PROXYPOOL_FIREWALL_DEFAULTS="$LIVE_DIR/defaults-fail" \
	PROXYPOOL_COLD_WRAPPER_TEMPLATE="$COLD_WRAPPER" \
	PROXYPOOL_UCI_DEFAULTS_DIR="$LIVE_DIR/uci-defaults" \
	PROXYPOOL_SYNC="$SYNC" \
	sh "$POSTINST" 2>"$TEST_TMP/live-deferred.err"
grep -Fq 'reboot immediately' "$TEST_TMP/live-deferred.err" || {
	echo 'deferred live post-install did not require an immediate reboot' >&2
	exit 1
}
grep -Fq 'do not connect clients' "$TEST_TMP/live-deferred.err" || {
	echo 'deferred live post-install did not disclose its unsafe pre-reboot window' >&2
	exit 1
}
LIVE_SCHEDULED="$LIVE_DIR/uci-defaults/99-proxypool-safety"
[ -f "$LIVE_SCHEDULED" ] && [ ! -L "$LIVE_SCHEDULED" ] || {
	echo 'failed live post-install did not schedule the cold fallback' >&2
	exit 1
}
cmp -s "$COLD_WRAPPER" "$LIVE_SCHEDULED" || {
	echo 'failed live post-install scheduled the wrong content' >&2
	exit 1
}
[ "$(stat -c '%a' "$LIVE_SCHEDULED")" = 755 ] || {
	echo 'failed live post-install scheduled the wrong mode' >&2
	exit 1
}

# The cold wrapper must remain failed while a durable WAL exists.  It may only
# return success without reapplying defaults after the validated live config is
# visible and the S19 finalizer has retired that WAL.
COLD_DIR="$TEST_TMP/cold"
mkdir -p "$COLD_DIR/config"
printf '%s\n' data >"$COLD_DIR/config/firewall"
printf '%s\n' data >"$COLD_DIR/config/dhcp"
printf '%s\n' data >"$COLD_DIR/config/network"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	'case "$1" in' \
	'  journal-present) [ -e "$PROXYPOOL_TEST_JOURNAL" ] ;;' \
	'  activation-current) [ -e "$PROXYPOOL_TEST_ACTIVATION_CURRENT" ] ;;' \
	'  *) exit 2 ;;' \
	'esac' >"$COLD_DIR/transaction"
chmod 755 "$COLD_DIR/transaction"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	'[ -e "$PROXYPOOL_TEST_CONFIG_VALID" ]' >"$COLD_DIR/check"
chmod 755 "$COLD_DIR/check"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	'printf "%s\\n" "${PROXYPOOL_COLD_BOOT:-unset}" >>"$PROXYPOOL_TEST_DEFAULTS_LOG"' \
	': >"$PROXYPOOL_TEST_JOURNAL"' \
	'exit 0' >"$COLD_DIR/defaults"
chmod 755 "$COLD_DIR/defaults"

run_cold_wrapper() {
	PROXYPOOL_CONFIG_DIR="$COLD_DIR/config" \
		PROXYPOOL_FIREWALL_DEFAULTS="$COLD_DIR/defaults" \
		PROXYPOOL_TRANSACTION_HELPER="$COLD_DIR/transaction" \
		PROXYPOOL_FIREWALL_TRANSACTION_DIR="$COLD_DIR/journal" \
		PROXYPOOL_FW4_CHECK="$COLD_DIR/check" \
		PROXYPOOL_TEST_JOURNAL="$COLD_DIR/journal" \
		PROXYPOOL_TEST_ACTIVATION_CURRENT="$COLD_DIR/activation-current" \
		PROXYPOOL_TEST_CONFIG_VALID="$COLD_DIR/config-valid" \
		PROXYPOOL_TEST_DEFAULTS_LOG="$COLD_DIR/defaults.log" \
		sh "$COLD_WRAPPER"
}

: >"$COLD_DIR/journal"
: >"$COLD_DIR/defaults.log"
if run_cold_wrapper; then
	echo 'cold wrapper succeeded while the firewall journal was pending' >&2
	exit 1
fi
[ ! -s "$COLD_DIR/defaults.log" ] || {
	echo 'cold wrapper reapplied defaults over a pending firewall journal' >&2
	exit 1
}

# Simulate a cross-boot S18 recovery: WAL is gone and the post-clamp firewall
# renders successfully, but no S19 runtime acknowledgement marker exists.  The
# wrapper must retry instead of deleting itself on checker output alone.
rm -f "$COLD_DIR/journal"
: >"$COLD_DIR/config-valid"
if run_cold_wrapper; then
	echo 'cold wrapper mistook a recovered post-clamp baseline for completed S19 activation' >&2
	exit 1
fi
[ "$(cat "$COLD_DIR/defaults.log")" = 1 ] || {
	echo 'cold wrapper did not invoke defaults exactly in cold mode' >&2
	exit 1
}

rm -f "$COLD_DIR/journal"
: >"$COLD_DIR/activation-current"
if ! run_cold_wrapper; then
	echo 'cold wrapper did not retire after validated finalization' >&2
	exit 1
fi
[ "$(wc -l <"$COLD_DIR/defaults.log" | tr -d '[:space:]')" = 1 ] || {
	echo 'cold wrapper reapplied already-finalized defaults' >&2
	exit 1
}

echo 'package safety integration: PASS'
