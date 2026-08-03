#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
DEFAULT_DIR="$ROOT/luci-app-proxypool/root/etc/uci-defaults"
DEFAULT="$DEFAULT_DIR/luci-proxypool"
MAIN_VIEW="$ROOT/luci-app-proxypool/luasrc/view/proxypool/main.htm"
MAKEFILE="$ROOT/luci-app-proxypool/Makefile"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM
TEST_SHELL=$(command -v sh)

fail() {
	printf 'LuCI package source safety: %s\n' "$*" >&2
	exit 1
}

[ -f "$DEFAULT" ] && [ ! -L "$DEFAULT" ] || fail 'missing regular luci-proxypool uci-default'
[ "$(find "$DEFAULT_DIR" -mindepth 1 -maxdepth 1 -print | wc -l | tr -d ' ')" -eq 1 ] ||
	fail 'only the packaged luci-proxypool uci-default may remain in source'

# Package installation must never provision the router or patch files owned by
# another package.  Reject the historical side effects before executing the
# default in the behavioral fixture below.
if grep -Eiq 'xl2tpd|(^|[^[:alnum:]_])(network|firewall|wireless)\.|wifi([[:space:]]+(config|reload|up))?|encryption=.?(none)|/www/|luci\.js|cascade\.css|header\.htm|/etc/init\.d/|uhttpd|sed[[:space:]]+-i' "$DEFAULT"; then
	fail 'uci-default contains router provisioning or global LuCI mutation'
fi

[ "$(grep -Fc '<link rel="stylesheet" href="<%=resource%>/proxypool-v2.css" />' "$MAIN_VIEW")" -eq 1 ] ||
	fail 'main view must load the packaged ProxyPool stylesheet exactly once'
[ "$(grep -Fc '<script type="text/javascript" src="<%=resource%>/proxypool-v2.js"></script>' "$MAIN_VIEW")" -eq 1 ] ||
	fail 'main view must load the packaged ProxyPool script exactly once'

if grep -Fq 'luci-proxypool-menu' "$MAKEFILE"; then
	fail 'legacy global-menu mutator must not be packaged'
fi
[ "$(grep -Fxc 'FILE_MODES:=/etc/uci-defaults/luci-proxypool:root:root:0755' "$MAKEFILE" || true)" -eq 1 ] ||
	fail 'LuCI package must pin its uci-default mode through the OpenWrt IPK builder'
[ "$(grep -Fxc '# call BuildPackage - OpenWrt buildroot signature' "$MAKEFILE" || true)" -eq 1 ] ||
	fail 'LuCI package must expose the standard buildroot scanner signature exactly once'
if grep -Eq '^define[[:space:]]+Package/luci-app-proxypool/install' "$MAKEFILE" ||
	grep -Fqx '$(eval $(call BuildPackage,luci-app-proxypool))' "$MAKEFILE"; then
	fail 'LuCI package must use the single install/build path owned by luci.mk'
fi

mkdir -p "$TEST_TMP/bin"
cat >"$TEST_TMP/bin/uci" <<'EOF'
#!/bin/sh
set -eu
printf 'args:%s\n' "$*" >"$PROXYPOOL_TEST_UCI_TRACE"
while IFS= read -r line; do
	printf '%s\n' "$line" >>"$PROXYPOOL_TEST_UCI_TRACE"
done
EOF
chmod 755 "$TEST_TMP/bin/uci"
cat >"$TEST_TMP/bin/rm" <<'EOF'
#!/bin/sh
set -eu
printf 'args:%s\n' "$*" >>"$PROXYPOOL_TEST_RM_TRACE"
EOF
chmod 755 "$TEST_TMP/bin/rm"

PROXYPOOL_LUCI_CACHE_ROOT=/proxypool-test-cache-root \
PROXYPOOL_TEST_UCI_TRACE="$TEST_TMP/uci.trace" \
PROXYPOOL_TEST_RM_TRACE="$TEST_TMP/rm.trace" \
PATH="$TEST_TMP/bin" \
	"$TEST_SHELL" "$DEFAULT"

cat >"$TEST_TMP/expected.trace" <<'EOF'
args:-q batch
delete ucitrack.@proxypool[-1]
add ucitrack proxypool
set ucitrack.@proxypool[-1].init='proxypool'
commit ucitrack
EOF
cmp -s "$TEST_TMP/expected.trace" "$TEST_TMP/uci.trace" ||
	fail 'uci-default must make only the expected ucitrack transaction'
cat >"$TEST_TMP/expected-rm.trace" <<'EOF'
args:-f /tmp/luci-indexcache
args:-rf /tmp/luci-modulecache
EOF
cmp -s "$TEST_TMP/expected-rm.trace" "$TEST_TMP/rm.trace" ||
	fail 'uci-default must invalidate only the two LuCI caches below /tmp'

echo 'LuCI package source safety: PASS'
