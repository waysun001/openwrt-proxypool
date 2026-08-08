#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
INSPECTOR="$ROOT/scripts/inspect-luci-ipk.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

mkdir "$TEST_TMP/bin"
cat >"$TEST_TMP/bin/stat" <<'EOF'
#!/usr/bin/env sh
set -eu
target=
for argument in "$@"; do target=$argument; done
case "${PROXYPOOL_TEST_WRONG_MODE:-}:$target" in
	uci-default:*/etc/uci-defaults/luci-proxypool) printf '644\n'; exit 0 ;;
esac
case "$target" in
	*/etc/uci-defaults/luci-proxypool) printf '755\n' ;;
	*) printf '644\n' ;;
esac
EOF
chmod 755 "$TEST_TMP/bin/stat"

write_safe_default() {
	destination=$1
	cat >"$destination" <<'EOF'
#!/bin/sh
set -eu
uci -q batch <<'UCIEOF'
delete ucitrack.@proxypool[-1]
add ucitrack proxypool
set ucitrack.@proxypool[-1].init='proxypool'
commit ucitrack
UCIEOF
rm -f /tmp/luci-indexcache
rm -rf /tmp/luci-modulecache
exit 0
EOF
	chmod 755 "$destination"
}

write_main_view() {
	destination=$1
	cat >"$destination" <<'EOF'
<%+header%>
<link rel="stylesheet" href="<%=resource%>/proxypool-global.css" />
<link rel="stylesheet" href="<%=resource%>/proxypool-v2.css" />
<nav id="proxypool-global-menu"></nav>
<div id="pp-v2-binding-modal"><input id="pp-v2-binding-search" /><div id="pp-v2-binding-list"></div><button id="pp-v2-binding-save"></button></div>
<script type="text/javascript" src="<%=resource%>/proxypool-global.js"></script>
<script type="text/javascript" src="<%=resource%>/proxypool-v2.js"></script>
<div id="proxypool"></div>
<%+footer%>
EOF
}

make_ipk() {
	name=$1
	kind=$2
	fixture="$TEST_TMP/$name"
	outer="$fixture/outer"
	control="$fixture/control"
	data="$fixture/data"
	mkdir -p "$outer" "$control" \
		"$data/etc/uci-defaults" \
		"$data/usr/lib/lua/luci/controller" \
		"$data/usr/lib/lua/luci/model" \
		"$data/usr/lib/lua/luci/view/proxypool" \
		"$data/www/luci-static/resources"

	architecture=all
	depends='libc, proxypool-core, luci-base, luci-lua-runtime, luci-compat, luci-lib-jsonc'
	case "$kind" in
		bad_arch) architecture=aarch64_cortex-a53 ;;
		missing_core_dep) depends='libc, luci-base, luci-lua-runtime, luci-compat, luci-lib-jsonc' ;;
	esac
	printf '%s\n' \
		'Package: luci-app-proxypool' \
		'Version: 1.0.0-1' \
		"Architecture: $architecture" \
		"Depends: $depends" >"$control/control"

	write_safe_default "$data/etc/uci-defaults/luci-proxypool"
	printf '%s\n' 'return true' >"$data/usr/lib/lua/luci/controller/proxypool.lua"
	printf '%s\n' 'return { call = function() return {} end }' >"$data/usr/lib/lua/luci/model/proxypool_rpc.lua"
	write_main_view "$data/usr/lib/lua/luci/view/proxypool/main.htm"
	printf '<div>locked</div>\n' >"$data/usr/lib/lua/luci/view/proxypool/locked.htm"
	printf '<div>lease</div>\n' >"$data/usr/lib/lua/luci/view/proxypool/lease.htm"
	printf 'body{}\n' >"$data/www/luci-static/resources/proxypool-global.css"
	printf 'window.ProxyPool=true;\n' >"$data/www/luci-static/resources/proxypool-global.js"
	printf 'body{}\n' >"$data/www/luci-static/resources/proxypool-v2.css"
	printf 'window.ProxyPoolV2=true;\n' >"$data/www/luci-static/resources/proxypool-v2.js"

	case "$kind" in
		missing_css) rm -f "$data/www/luci-static/resources/proxypool-global.css" ;;
		extra_global_luci) printf 'mutated\n' >"$data/www/luci-static/resources/luci.js" ;;
		extra_uci_default) printf '#!/bin/sh\nexit 0\n' >"$data/etc/uci-defaults/luci-proxypool-menu" ;;
		unsafe_default) printf '#!/bin/sh\n/etc/init.d/xl2tpd disable\n' >"$data/etc/uci-defaults/luci-proxypool" ;;
		missing_resource_load) printf '<%%+header%%>\n<div></div>\n<%%+footer%%>\n' >"$data/usr/lib/lua/luci/view/proxypool/main.htm" ;;
		versioned_resource_load)
			printf '%s\n' \
				'<%+header%>' \
				'<link rel="stylesheet" href="<%=resource%>/proxypool-global.css?v=1.0.0" />' \
				'<link rel="stylesheet" href="<%=resource%>/proxypool-v2.css?v=1.0.0" />' \
				'<nav id="proxypool-global-menu"></nav>' \
				'<div id="pp-v2-binding-modal"><input id="pp-v2-binding-search" /><div id="pp-v2-binding-list"></div><button id="pp-v2-binding-save"></button></div>' \
				'<script type="text/javascript" src="<%=resource%>/proxypool-global.js?v=1.0.0"></script>' \
				'<script type="text/javascript" src="<%=resource%>/proxypool-v2.js?v=1.0.0"></script>' \
				'<%+footer%>' >"$data/usr/lib/lua/luci/view/proxypool/main.htm"
			;;
	esac

	chmod 644 "$data/usr/lib/lua/luci/controller/proxypool.lua" \
		"$data/usr/lib/lua/luci/model/proxypool_rpc.lua" \
		"$data/usr/lib/lua/luci/view/proxypool/main.htm" \
		"$data/usr/lib/lua/luci/view/proxypool/locked.htm" \
		"$data/usr/lib/lua/luci/view/proxypool/lease.htm" \
		"$data/www/luci-static/resources/proxypool-global.js"
	chmod 644 "$data/www/luci-static/resources/proxypool-v2.js" \
		"$data/www/luci-static/resources/proxypool-v2.css"
	[ ! -e "$data/www/luci-static/resources/proxypool-global.css" ] ||
		chmod 644 "$data/www/luci-static/resources/proxypool-global.css"

	printf '2.0\n' >"$outer/debian-binary"
	tar -czf "$outer/control.tar.gz" -C "$control" .
	tar -czf "$outer/data.tar.gz" -C "$data" .
	tar -czf "$TEST_TMP/$name.ipk" -C "$outer" .
}

run_inspector() {
	name=$1
	wrong_mode=${2:-}
	PROXYPOOL_TEST_WRONG_MODE="$wrong_mode" PATH="$TEST_TMP/bin:$PATH" \
		sh "$INSPECTOR" "$TEST_TMP/$name.ipk" >"$TEST_TMP/$name.log" 2>&1
}

make_ipk valid valid
if ! run_inspector valid; then
	cat "$TEST_TMP/valid.log" >&2
	exit 1
fi
grep -Fq 'LuCI IPK contents: PASS' "$TEST_TMP/valid.log"

make_ipk versioned_resource_load versioned_resource_load
if ! run_inspector versioned_resource_load; then
	cat "$TEST_TMP/versioned_resource_load.log" >&2
	exit 1
fi

for invalid_kind in bad_arch missing_core_dep missing_css extra_global_luci extra_uci_default unsafe_default missing_resource_load; do
	make_ipk "$invalid_kind" "$invalid_kind"
	if run_inspector "$invalid_kind"; then
		printf 'invalid LuCI IPK fixture passed: %s\n' "$invalid_kind" >&2
		exit 1
	fi
done

make_ipk wrong_mode valid
if run_inspector wrong_mode uci-default; then
	echo 'wrong LuCI uci-default mode passed inspection' >&2
	exit 1
fi

grep -Fq 'unexpected package architecture metadata' "$TEST_TMP/bad_arch.log"
grep -Fq 'missing required dependency: proxypool-core' "$TEST_TMP/missing_core_dep.log"
grep -Fq 'unexpected LuCI package payload' "$TEST_TMP/missing_css.log"
grep -Fq 'unexpected LuCI package payload' "$TEST_TMP/extra_global_luci.log"
grep -Fq 'unexpected LuCI package payload' "$TEST_TMP/extra_uci_default.log"
grep -Fq 'uci-default contains router provisioning or global LuCI mutation' "$TEST_TMP/unsafe_default.log"
grep -Fq 'main view does not load packaged V2 assets' "$TEST_TMP/missing_resource_load.log"
grep -Fq 'unexpected mode for /etc/uci-defaults/luci-proxypool' "$TEST_TMP/wrong_mode.log"

echo 'LuCI IPK fixture inspection: PASS'
