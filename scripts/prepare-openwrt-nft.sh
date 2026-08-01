#!/usr/bin/env sh
set -eu

umask 077

ROOTFS_URL='https://downloads.openwrt.org/releases/23.05.3/targets/x86/64/openwrt-23.05.3-x86-64-rootfs.tar.gz'
ROOTFS_SHA256='7a1dc79ebff5f6b6d4176369afa1fadce8ce9fe9d19f85a1d6e2a923c7db6036'
NFT_PACKAGE_VERSION='1.0.8-1'
NFT_RUNTIME_VERSION='nftables v1.0.8'
WORK_ROOT=

fail() {
	printf 'OpenWrt nft preparation: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	if [ -n "$WORK_ROOT" ] && [ -d "$WORK_ROOT" ] && [ ! -L "$WORK_ROOT" ]; then
		rm -rf -- "$WORK_ROOT" || status=1
	fi
	exit "$status"
}

terminate() {
	status=$1
	trap - EXIT HUP INT TERM
	if [ -n "$WORK_ROOT" ] && [ -d "$WORK_ROOT" ] && [ ! -L "$WORK_ROOT" ]; then
		rm -rf -- "$WORK_ROOT" >/dev/null 2>&1 || true
	fi
	exit "$status"
}

require_safe_destination() {
	destination=$1
	case "$destination" in
		/*) : ;;
		*) fail 'rootfs destination must be an absolute path' ;;
	esac
	[ "$destination" != / ] || fail 'rootfs destination must not be the filesystem root'
	printf '%s\n' "$destination" | grep -Eq '^/[A-Za-z0-9._/-]+$' ||
		fail 'rootfs destination contains unsupported characters'
	case "$destination" in
		*/../*|*/..|*/./*|*/.|*//*) fail 'rootfs destination must be canonical' ;;
	esac
	parent=$(dirname "$destination")
	name=$(basename "$destination")
	case "$name" in
		''|.|..) fail 'rootfs destination has an unsafe basename' ;;
	esac
	[ "$parent" != / ] || fail 'rootfs destination parent must not be the filesystem root'
	[ -d "$parent" ] && [ ! -L "$parent" ] ||
		fail "rootfs destination parent is missing or unsafe: $parent"
	resolved_parent=$(CDPATH= cd -- "$parent" && pwd -P) ||
		fail "cannot resolve rootfs destination parent: $parent"
	[ "$destination" = "$resolved_parent/$name" ] ||
		fail 'rootfs destination must use its canonical physical parent path'
	[ ! -e "$destination" ] && [ ! -L "$destination" ] ||
		fail "rootfs destination already exists: $destination"
}

[ "$#" -eq 1 ] || fail 'expected exactly one rootfs destination argument'
ROOTFS=$1
require_safe_destination "$ROOTFS"

for command_name in awk basename chroot curl dirname grep mkdir mktemp mv rm sha256sum sudo tar; do
	command -v "$command_name" >/dev/null 2>&1 ||
		fail "required command is unavailable: $command_name"
done
sudo -n true >/dev/null 2>&1 || fail 'passwordless sudo is required'

ROOTFS_PARENT=$(dirname "$ROOTFS")
WORK_ROOT=$(mktemp -d "$ROOTFS_PARENT/.proxypool-openwrt-nft.XXXXXX") ||
	fail 'cannot create rootfs preparation workspace'
trap cleanup EXIT
trap 'terminate 129' HUP
trap 'terminate 130' INT
trap 'terminate 143' TERM

ARCHIVE="$WORK_ROOT/rootfs.tar.gz"
STAGED_ROOTFS="$WORK_ROOT/rootfs"

curl --proto '=https' --tlsv1.2 --fail --location \
	--retry 3 --retry-all-errors --connect-timeout 20 \
	--output "$ARCHIVE" "$ROOTFS_URL" || fail 'cannot download pinned OpenWrt rootfs'

actual_sha256=$(sha256sum "$ARCHIVE" | awk '{ print $1 }') ||
	fail 'cannot hash downloaded OpenWrt rootfs'
[ "$actual_sha256" = "$ROOTFS_SHA256" ] ||
	fail "OpenWrt rootfs SHA256 mismatch: $actual_sha256"

mkdir "$STAGED_ROOTFS" || fail 'cannot create staged rootfs directory'
tar --extract --gzip --file="$ARCHIVE" --directory="$STAGED_ROOTFS" --no-same-owner ||
	fail 'cannot extract pinned OpenWrt rootfs'

NFT="$STAGED_ROOTFS/usr/sbin/nft"
NFT_CONTROL="$STAGED_ROOTFS/usr/lib/opkg/info/nftables-json.control"
[ -f "$NFT" ] && [ ! -L "$NFT" ] && [ -x "$NFT" ] ||
	fail 'pinned rootfs does not contain an executable regular /usr/sbin/nft'
[ -f "$NFT_CONTROL" ] && [ ! -L "$NFT_CONTROL" ] ||
	fail 'pinned rootfs does not contain nftables-json package metadata'
[ "$(grep -Fxc "Version: $NFT_PACKAGE_VERSION" "$NFT_CONTROL" || true)" -eq 1 ] ||
	fail "pinned rootfs does not contain nftables-json $NFT_PACKAGE_VERSION"

nft_version=$(sudo -n chroot "$STAGED_ROOTFS" /usr/sbin/nft --version) ||
	fail 'pinned OpenWrt nft executable cannot run under chroot'
case "$nft_version" in
	"$NFT_RUNTIME_VERSION"|"$NFT_RUNTIME_VERSION "*) : ;;
	*) fail "unexpected pinned OpenWrt nft version: $nft_version" ;;
esac

[ ! -e "$ROOTFS" ] && [ ! -L "$ROOTFS" ] ||
	fail "rootfs destination appeared during preparation: $ROOTFS"
mv -T "$STAGED_ROOTFS" "$ROOTFS" || fail 'cannot publish verified OpenWrt rootfs'
printf 'OpenWrt nft preparation: verified %s (%s)\n' "$ROOTFS" "$nft_version"
