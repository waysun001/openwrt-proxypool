#!/usr/bin/env sh
set -eu

umask 077

ROOTFS="${PROXYPOOL_OPENWRT_ROOTFS:-}"
RULESET_COPY=

fail() {
	printf 'OpenWrt target nft: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	if [ -n "$RULESET_COPY" ]; then
		rm -f -- "$RULESET_COPY" || status=1
	fi
	exit "$status"
}

terminate() {
	status=$1
	trap - EXIT HUP INT TERM
	if [ -n "$RULESET_COPY" ]; then
		rm -f -- "$RULESET_COPY" >/dev/null 2>&1 || true
	fi
	exit "$status"
}

[ "$#" -eq 3 ] || fail 'expected exactly: -c -f RULESET'
[ "$1" = -c ] && [ "$2" = -f ] || fail 'only -c -f RULESET is supported'
HOST_RULESET=$3

case "$ROOTFS" in
	/*) : ;;
	*) fail 'PROXYPOOL_OPENWRT_ROOTFS must be an absolute path' ;;
esac
[ "$ROOTFS" != / ] || fail 'PROXYPOOL_OPENWRT_ROOTFS must not be the filesystem root'
[ -d "$ROOTFS" ] && [ ! -L "$ROOTFS" ] || fail 'OpenWrt rootfs is missing or unsafe'
resolved_rootfs=$(CDPATH= cd -- "$ROOTFS" && pwd -P) || fail 'cannot resolve OpenWrt rootfs'
[ "$ROOTFS" = "$resolved_rootfs" ] || fail 'OpenWrt rootfs path must be canonical'
[ -f "$ROOTFS/usr/sbin/nft" ] && [ ! -L "$ROOTFS/usr/sbin/nft" ] &&
	[ -x "$ROOTFS/usr/sbin/nft" ] || fail 'OpenWrt nft executable is missing or unsafe'
[ -d "$ROOTFS/tmp" ] && [ ! -L "$ROOTFS/tmp" ] || fail 'OpenWrt rootfs /tmp is missing or unsafe'

case "$HOST_RULESET" in
	/*) : ;;
	*) fail 'ruleset path must be absolute' ;;
esac
[ -f "$HOST_RULESET" ] && [ ! -L "$HOST_RULESET" ] && [ -r "$HOST_RULESET" ] ||
	fail 'ruleset is missing or unsafe'

for command_name in chmod chroot cp mktemp rm sudo unshare; do
	command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done
sudo -n true >/dev/null 2>&1 || fail 'passwordless sudo is required'

RULESET_COPY=$(mktemp "$ROOTFS/tmp/proxypool-nft-check.XXXXXX") ||
	fail 'cannot create private ruleset copy inside OpenWrt rootfs'
trap cleanup EXIT
trap 'terminate 129' HUP
trap 'terminate 130' INT
trap 'terminate 143' TERM

cp -- "$HOST_RULESET" "$RULESET_COPY" || fail 'cannot copy ruleset into OpenWrt rootfs'
chmod 600 "$RULESET_COPY" || fail 'cannot protect copied ruleset'
CHROOT_RULESET="/tmp/${RULESET_COPY##*/}"

# A private network namespace avoids inspecting or touching the runner's host
# firewall.  nft check mode validates the exact pinned userspace parser and the
# Ubuntu runner kernel without committing the transaction.
sudo -n unshare --net -- chroot "$ROOTFS" /usr/sbin/nft -c -f "$CHROOT_RULESET"
