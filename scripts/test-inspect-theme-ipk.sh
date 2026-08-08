#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
INSPECTOR="$ROOT/scripts/inspect-theme-ipk.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

write_default() {
	destination=$1
	cat >"$destination" <<'EOF'
#!/bin/sh
theme_root=${IPKG_INSTROOT:-}/usr/share/ucode/luci/template/themes/proxypool
asset_root=${IPKG_INSTROOT:-}/www/luci-static/proxypool
[ -f "$theme_root/header.ut" ] && [ -f "$theme_root/footer.ut" ] && [ -f "$asset_root/proxypool-global.css" ] && [ -f "$asset_root/proxypool-global.js" ] || exit 0
uci -q set luci.themes.ProxyPool='/luci-static/proxypool'
uci -q set luci.main.mediaurlbase='/luci-static/proxypool'
uci -q commit luci
EOF
}

write_postrm() {
	destination=$1
	cat >"$destination" <<'EOF'
#!/bin/sh
active="$(uci -q get luci.main.mediaurlbase 2>/dev/null || true)"
[ "$active" != '/luci-static/proxypool' ] || uci -q set luci.main.mediaurlbase='/luci-static/bootstrap'
uci -q delete luci.themes.ProxyPool
uci -q commit luci
rm -f /tmp/luci-indexcache
rm -rf /tmp/luci-modulecache
EOF
}

make_ipk() {
	name=$1
	kind=$2
	fixture="$TEST_TMP/$name"
	outer="$fixture/outer"
	control="$fixture/control"
	data="$fixture/data"
	mkdir -p "$outer" "$control" "$data/etc/uci-defaults" \
		"$data/usr/share/ucode/luci/template/themes/proxypool" \
		"$data/www/luci-static/proxypool"

	architecture=all
	depends='libc, luci-base, luci-theme-bootstrap'
	case "$kind" in
		bad_arch) architecture=aarch64_cortex-a53 ;;
		missing_dep) depends='libc, luci-base' ;;
	esac
	printf '%s\n' 'Package: luci-theme-proxypool' 'Version: 2.1.0-1' \
		"Architecture: $architecture" "Depends: $depends" >"$control/control"
	write_postrm "$control/postrm"
	write_default "$data/etc/uci-defaults/30_luci-theme-proxypool"
	printf '%s\n' '{# Licensed to the public under the Apache License 2.0. -#}' >"$data/usr/share/ucode/luci/template/themes/proxypool/header.ut"
	printf '%s\n' '</body>' >"$data/usr/share/ucode/luci/template/themes/proxypool/footer.ut"
	printf '%s\n' '<section></section>' >"$data/usr/share/ucode/luci/template/themes/proxypool/sysauth.ut"
	printf '%s\n' '#proxypool-global-menu{}' >"$data/www/luci-static/proxypool/proxypool-global.css"
	printf '%s\n' 'window.ProxyPoolTheme=true;' >"$data/www/luci-static/proxypool/proxypool-global.js"

	case "$kind" in
		missing_file) rm -f "$data/usr/share/ucode/luci/template/themes/proxypool/footer.ut" ;;
		extra_bootstrap)
			mkdir -p "$data/www/luci-static/bootstrap"
			printf 'bad\n' >"$data/www/luci-static/bootstrap/cascade.css"
			;;
		unsafe_default) printf '%s\n' '#!/bin/sh' 'sed -i s/x/y/ /www/luci-static/bootstrap/cascade.css' >"$data/etc/uci-defaults/30_luci-theme-proxypool" ;;
		bad_postrm) printf '%s\n' '#!/bin/sh' 'exit 0' >"$control/postrm" ;;
		symlink) rm -f "$data/www/luci-static/proxypool/proxypool-global.js"; ln -s proxypool-global.css "$data/www/luci-static/proxypool/proxypool-global.js" ;;
	esac

	chmod 644 "$control/control"
	chmod 755 "$control/postrm" "$data/etc/uci-defaults/30_luci-theme-proxypool"
	find "$data" -type f ! -path '*/etc/uci-defaults/*' -exec chmod 644 {} +
	[ "$kind" != wrong_mode ] || chmod 664 "$data/usr/share/ucode/luci/template/themes/proxypool/header.ut"

	printf '2.0\n' >"$outer/debian-binary"
	tar -czf "$outer/control.tar.gz" -C "$control" .
	tar -czf "$outer/data.tar.gz" -C "$data" .
	tar -czf "$TEST_TMP/$name.ipk" -C "$outer" .
}

make_ipk valid valid
sh "$INSPECTOR" "$TEST_TMP/valid.ipk" >"$TEST_TMP/valid.log" 2>&1 || { cat "$TEST_TMP/valid.log" >&2; exit 1; }
grep -Fq 'ProxyPool theme IPK contents: PASS' "$TEST_TMP/valid.log"

for kind in bad_arch missing_dep missing_file extra_bootstrap unsafe_default bad_postrm symlink wrong_mode; do
	make_ipk "$kind" "$kind"
	if sh "$INSPECTOR" "$TEST_TMP/$kind.ipk" >"$TEST_TMP/$kind.log" 2>&1; then
		echo "invalid theme IPK fixture passed: $kind" >&2
		exit 1
	fi
done

grep -Fq 'unexpected theme package architecture metadata' "$TEST_TMP/bad_arch.log"
grep -Fq 'missing required theme dependency: luci-theme-bootstrap' "$TEST_TMP/missing_dep.log"
grep -Fq 'unexpected theme package payload' "$TEST_TMP/missing_file.log"
grep -Fq 'theme package owns Bootstrap paths' "$TEST_TMP/extra_bootstrap.log" || grep -Fq 'unexpected theme package payload' "$TEST_TMP/extra_bootstrap.log"
grep -Fq 'theme package mutates Bootstrap-owned files' "$TEST_TMP/unsafe_default.log" || grep -Fq 'theme activation contract missing' "$TEST_TMP/unsafe_default.log"
grep -Fq 'theme postrm contract missing' "$TEST_TMP/bad_postrm.log"
grep -Fq 'theme package must not contain symbolic links' "$TEST_TMP/symlink.log"
grep -Fq 'unexpected mode for /usr/share/ucode/luci/template/themes/proxypool/header.ut' "$TEST_TMP/wrong_mode.log"

echo 'ProxyPool theme IPK fixture inspection: PASS'
