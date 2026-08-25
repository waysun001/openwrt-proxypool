#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
PREPARE="$ROOT/scripts/prepare-image-files.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM
SOURCE="$TEST_TMP/source"
DESTINATION="$TEST_TMP/staged"

# A Linux checkout is itself a supported input to the full-source firmware
# build.  Keep the netifd handler executable even when a caller copies the
# overlay directly instead of first normalizing it through the staging helper.
case "$(uname -s)" in
	Linux*)
		CHECKOUT="$TEST_TMP/repository-checkout/"
		mkdir -p "$CHECKOUT"
		git -C "$ROOT" checkout-index --prefix="$CHECKOUT" -- files/lib/netifd/proto/l2tp.sh
		[ -x "$CHECKOUT/files/lib/netifd/proto/l2tp.sh" ] || {
			echo 'repository checkout leaves the L2TP netifd protocol handler non-executable' >&2
			exit 1
		}
		;;
esac

mkdir -p \
	"$SOURCE/etc/config" \
	"$SOURCE/etc/uci-defaults" \
	"$SOURCE/etc/proxypool" \
	"$SOURCE/lib/netifd/proto" \
	"$SOURCE/usr/lib/proxypool"
printf '%s\n' "config global 'global'" >"$SOURCE/etc/config/proxypool"
printf '%s\n' "config global 'global'" >"$SOURCE/etc/config/proxypool_v2"
printf '%s\n' "config global 'global'" >"$SOURCE/etc/config/proxypool_runtime"
printf '%s\n' image >"$SOURCE/etc/proxypool/v2-activation-request"
printf '%s\n' v2-image-activation-v1 >"$SOURCE/usr/lib/proxypool/v2-image-activation-authority"
printf '%s\n' '#!/bin/sh' 'exit 0' >"$SOURCE/etc/uci-defaults/keep"
printf '%s\n' '#!/bin/sh' 'exit 0' >"$SOURCE/lib/netifd/proto/l2tp.sh"
chmod 644 "$SOURCE/etc/config/proxypool"
chmod 644 "$SOURCE/etc/config/proxypool_v2"
chmod 644 "$SOURCE/etc/config/proxypool_runtime"
chmod 755 "$SOURCE/etc/uci-defaults/keep"
chmod 644 "$SOURCE/lib/netifd/proto/l2tp.sh"

sh "$PREPARE" "$SOURCE" "$DESTINATION"

cmp -s "$SOURCE/etc/config/proxypool" "$DESTINATION/etc/config/proxypool"
cmp -s "$SOURCE/etc/config/proxypool_v2" "$DESTINATION/etc/config/proxypool_v2"
cmp -s "$SOURCE/etc/config/proxypool_runtime" "$DESTINATION/etc/config/proxypool_runtime"
cmp -s "$SOURCE/etc/proxypool/v2-activation-request" "$DESTINATION/etc/proxypool/v2-activation-request"
cmp -s \
	"$SOURCE/usr/lib/proxypool/v2-image-activation-authority" \
	"$DESTINATION/usr/lib/proxypool/v2-image-activation-authority"
cmp -s "$SOURCE/etc/uci-defaults/keep" "$DESTINATION/etc/uci-defaults/keep"
cmp -s "$SOURCE/lib/netifd/proto/l2tp.sh" "$DESTINATION/lib/netifd/proto/l2tp.sh"
[ "$(stat -c '%a' "$SOURCE/etc/config/proxypool")" = 644 ]
[ "$(stat -c '%a' "$SOURCE/etc/config/proxypool_v2")" = 644 ]
[ "$(stat -c '%a' "$SOURCE/etc/config/proxypool_runtime")" = 644 ]
[ "$(stat -c '%a' "$DESTINATION/etc/uci-defaults/keep")" = 755 ]
case "$(uname -s)" in
	Linux*)
		for config_name in proxypool proxypool_v2 proxypool_runtime; do
			[ "$(stat -c '%a' "$DESTINATION/etc/config/$config_name")" = 600 ]
		done
		[ "$(stat -c '%a' "$DESTINATION/etc/proxypool")" = 700 ]
		[ "$(stat -c '%a' "$DESTINATION/etc/proxypool/v2-activation-request")" = 600 ]
		[ "$(stat -c '%a' "$DESTINATION/lib/netifd/proto/l2tp.sh")" = 755 ]
		;;
	*) echo 'SKIP: staged config mode assertion requires a POSIX filesystem' ;;
esac

for required_config in proxypool proxypool_v2 proxypool_runtime; do
	missing_source="$TEST_TMP/missing-$required_config"
	missing_destination="$TEST_TMP/staged-missing-$required_config"
	cp -a "$SOURCE" "$missing_source"
	rm "$missing_source/etc/config/$required_config"
	if sh "$PREPARE" "$missing_source" "$missing_destination" >/dev/null 2>&1; then
		printf 'staging accepted a missing required config: %s\n' "$required_config" >&2
		exit 1
	fi
	[ ! -e "$missing_destination" ] || {
		printf 'failed staging left a partial destination: %s\n' "$required_config" >&2
		exit 1
	}
done

missing_request_source="$TEST_TMP/missing-request"
missing_request_destination="$TEST_TMP/staged-missing-request"
cp -a "$SOURCE" "$missing_request_source"
rm "$missing_request_source/etc/proxypool/v2-activation-request"
if sh "$PREPARE" "$missing_request_source" "$missing_request_destination" >/dev/null 2>&1; then
	echo 'staging accepted a missing V2 activation request' >&2
	exit 1
fi
[ ! -e "$missing_request_destination" ] || {
	echo 'failed activation-request staging left a partial destination' >&2
	exit 1
}

missing_authorization_source="$TEST_TMP/missing-image-authorization"
missing_authorization_destination="$TEST_TMP/staged-missing-image-authorization"
cp -a "$SOURCE" "$missing_authorization_source"
rm "$missing_authorization_source/usr/lib/proxypool/v2-image-activation-authority"
if sh "$PREPARE" "$missing_authorization_source" "$missing_authorization_destination" >/dev/null 2>&1; then
	echo 'staging accepted a missing full-image activation authorization' >&2
	exit 1
fi
[ ! -e "$missing_authorization_destination" ] || {
	echo 'failed image-authorization staging left a partial destination' >&2
	exit 1
}

# Model a settings-preserving sysupgrade from a legacy-only V1 image. The old
# backup has no selector, so the new ROM default remains visible after restore
# and must not silently select V2. The separate V2 shadow config still ships so
# a later explicit migration has a validated target.
ROM="$TEST_TMP/rom"
OLD_V1_BACKUP="$TEST_TMP/old-v1-backup"
UPGRADED_V1="$TEST_TMP/upgraded-v1"
sh "$PREPARE" "$ROOT/files" "$ROM"
[ "$(stat -c '%a' "$ROM/lib/netifd/proto/l2tp.sh")" = 755 ]
mkdir -p "$OLD_V1_BACKUP/etc/config" "$UPGRADED_V1"
cp "$ROOT/proxypool-core/files/proxypool.config" "$OLD_V1_BACKUP/etc/config/proxypool"
cp -a "$ROM/." "$UPGRADED_V1/"
cp -a "$OLD_V1_BACKUP/." "$UPGRADED_V1/"
grep -Fq "option runtime_backend 'v1'" "$UPGRADED_V1/etc/config/proxypool_runtime" || {
	echo 'legacy V1 sysupgrade inherited a non-V1 ROM selector' >&2
	exit 1
}
if grep -Fq "option runtime_backend 'v2_shadow'" "$UPGRADED_V1/etc/config/proxypool_runtime"; then
	echo 'legacy V1 sysupgrade silently selected V2 shadow' >&2
	exit 1
fi
grep -Fq "option schema_version '2'" "$UPGRADED_V1/etc/config/proxypool_v2"
grep -Fq "option runtime_backend 'v2_shadow'" "$UPGRADED_V1/etc/config/proxypool_v2"

# New images preserve durable V2 user data, not generated selector, ownership,
# cleanup, activation, firewall, or wireless transaction state.  Model the
# keep.d selection itself so this fails if any transient path is reintroduced.
V2_OVERLAY="$TEST_TMP/v2-overlay"
V2_KEEP_BACKUP="$TEST_TMP/v2-keep-backup"
UPGRADED_V2="$TEST_TMP/upgraded-v2"
mkdir -p \
	"$V2_OVERLAY/etc/config" \
	"$V2_OVERLAY/etc/proxypool/backups" \
	"$V2_KEEP_BACKUP" \
	"$UPGRADED_V2"
printf '%s\n' "config global 'global'" "\toption schema_version '2'" "\toption runtime_backend 'v2_shadow'" \
	>"$V2_OVERLAY/etc/config/proxypool_v2"
printf '%s\n' migrated >"$V2_OVERLAY/etc/proxypool/migration-v1.json"
printf '%s\n' backup >"$V2_OVERLAY/etc/proxypool/backups/config.tar.gz"
printf "config global 'global'\n\toption runtime_backend 'v2_shadow'\n" >"$V2_OVERLAY/etc/config/proxypool_runtime"
printf '%s\n' v2_shadow >"$V2_OVERLAY/etc/proxypool/activated-backend"
printf '%s\n' v2_shadow >"$V2_OVERLAY/etc/proxypool/cleanup-required"
printf '%s\n' stale-image >"$V2_OVERLAY/etc/proxypool/v2-activation-request"
printf '%s\n' transaction >"$V2_OVERLAY/etc/proxypool/firewall-transaction"
printf '%s\n' quarantine >"$V2_OVERLAY/etc/proxypool/wireless-quarantine"

while IFS= read -r keep_path; do
	relative=${keep_path#/}
	source_path="$V2_OVERLAY/$relative"
	destination_path="$V2_KEEP_BACKUP/$relative"
	if [ -d "$source_path" ]; then
		mkdir -p "$destination_path"
		cp -a "$source_path/." "$destination_path/"
	elif [ -f "$source_path" ]; then
		mkdir -p "${destination_path%/*}"
		cp -a "$source_path" "$destination_path"
	fi
done <"$ROOT/proxypool-core/files/proxypool.keep"

cp -a "$ROM/." "$UPGRADED_V2/"
cp -a "$V2_KEEP_BACKUP/." "$UPGRADED_V2/"
cmp -s "$V2_OVERLAY/etc/config/proxypool_v2" "$UPGRADED_V2/etc/config/proxypool_v2" || {
	echo 'V2 user configuration was not selected by the keep policy' >&2
	exit 1
}
cmp -s "$V2_OVERLAY/etc/proxypool/migration-v1.json" "$UPGRADED_V2/etc/proxypool/migration-v1.json"
cmp -s "$V2_OVERLAY/etc/proxypool/backups/config.tar.gz" "$UPGRADED_V2/etc/proxypool/backups/config.tar.gz"
grep -Fq "option runtime_backend 'v1'" "$UPGRADED_V2/etc/config/proxypool_runtime" || {
	echo 'generated V2 selector leaked through the new keep policy' >&2
	exit 1
}
[ "$(cat "$UPGRADED_V1/etc/proxypool/v2-activation-request")" = image ] || {
	echo 'legacy V1 sysupgrade did not retain the explicit cold V2 activation request' >&2
	exit 1
}
[ "$(cat "$UPGRADED_V2/etc/proxypool/v2-activation-request")" = image ] || {
	echo 'new ROM image request was overridden by transient overlay state' >&2
	exit 1
}
for transient_path in \
	activated-backend \
	cleanup-required \
	firewall-transaction \
	wireless-quarantine; do
	[ ! -e "$UPGRADED_V2/etc/proxypool/$transient_path" ] || {
		echo "transient state leaked through the new keep policy: $transient_path" >&2
		exit 1
	}
done

echo 'ImageBuilder FILES staging: PASS'
