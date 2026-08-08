#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
THEME="$ROOT/luci-theme-proxypool"
MAKEFILE="$THEME/Makefile"
DEFAULT="$THEME/root/etc/uci-defaults/30_luci-theme-proxypool"
HEADER="$THEME/ucode/template/themes/proxypool/header.ut"
FOOTER="$THEME/ucode/template/themes/proxypool/footer.ut"
SYSAUTH="$THEME/ucode/template/themes/proxypool/sysauth.ut"
CSS="$THEME/htdocs/luci-static/proxypool/proxypool-global.css"
JS="$THEME/htdocs/luci-static/proxypool/proxypool-global.js"

fail() {
	printf 'ProxyPool theme source safety: %s\n' "$*" >&2
	exit 1
}

for file in "$MAKEFILE" "$DEFAULT" "$HEADER" "$FOOTER" "$SYSAUTH" "$CSS" "$JS"; do
	[ -f "$file" ] && [ ! -L "$file" ] || fail "missing regular file: ${file#$ROOT/}"
done
if find "$THEME" -type l -print -quit | grep -q .; then
	fail 'theme source must not contain symbolic links'
fi

grep -Fq 'LUCI_DEPENDS:=+luci-base +luci-theme-bootstrap' "$MAKEFILE" || fail 'theme dependencies are incomplete'
grep -Fq 'PKG_LICENSE:=Apache-2.0' "$MAKEFILE" || fail 'theme license is not pinned'
for mode_contract in \
	'/etc/uci-defaults/30_luci-theme-proxypool:root:root:0755' \
	'/usr/share/ucode/luci/template/themes/proxypool/header.ut:root:root:0644' \
	'/usr/share/ucode/luci/template/themes/proxypool/footer.ut:root:root:0644' \
	'/usr/share/ucode/luci/template/themes/proxypool/sysauth.ut:root:root:0644' \
	'/www/luci-static/proxypool/proxypool-global.css:root:root:0644' \
	'/www/luci-static/proxypool/proxypool-global.js:root:root:0644'; do
	grep -Fq "$mode_contract" "$MAKEFILE" || fail "missing package mode: $mode_contract"
done

grep -Fq 'Licensed to the public under the Apache License 2.0.' "$HEADER" || fail 'header lost the pinned Apache notice'
grep -Fq "{% if (!blank_page): %}" "$HEADER" || fail 'header does not respect blank pages'
for route in \
	"dispatcher.build_url('admin', 'services', 'proxypool')" \
	"dispatcher.build_url('admin', 'network')" \
	"dispatcher.build_url('admin', 'network', 'wireless')" \
	"dispatcher.build_url('admin', 'system')" \
	"dispatcher.build_url('admin', 'system', 'flash')" \
	"dispatcher.build_url('admin', 'system', 'reboot')"; do
	grep -Fq "$route" "$HEADER" || fail "missing navigation route: $route"
done
for marker in 'id="proxypool-global-menu"' 'aria-label="ProxyPool 管理导航"' 'aria-current="page"' \
	'id="pp-stat-total"' 'id="pp-stat-connected"' 'id="pp-stat-disconnected"' \
	'/luci-static/proxypool/proxypool-global.css' '/luci-static/proxypool/proxypool-global.js' 'ctx.request_path'; do
	grep -Fq "$marker" "$HEADER" || fail "header is missing: $marker"
done
grep -Fq ':focus-visible' "$CSS" || fail 'theme lacks keyboard focus styling'
grep -Fq '@media' "$CSS" || fail 'theme lacks mobile styling'
grep -Fq "getAttribute('data-status-url')" "$JS" || fail 'theme script does not use the generated status URL'
grep -Fq "document.getElementById('pp-stat-total')" "$JS" || fail 'theme script does not update status counters'

for contract in \
	"luci.themes.ProxyPool='/luci-static/proxypool'" \
	"luci.main.mediaurlbase='/luci-static/proxypool'" \
	'theme_root=${IPKG_INSTROOT:-}/usr/share/ucode/luci/template/themes/proxypool' \
	'asset_root=${IPKG_INSTROOT:-}/www/luci-static/proxypool'; do
	grep -Fq "$contract" "$DEFAULT" || fail "activation contract is missing: $contract"
done
for required in 'header.ut' 'footer.ut' 'proxypool-global.css' 'proxypool-global.js'; do
	grep -Fq "$required" "$DEFAULT" || fail "activation does not verify $required"
done
for contract in \
	"/luci-static/proxypool" \
	"/luci-static/bootstrap" \
	'uci -q delete luci.themes.ProxyPool' \
	'uci -q commit luci' \
	'/tmp/luci-indexcache' \
	'/tmp/luci-modulecache'; do
	grep -Fq "$contract" "$MAKEFILE" || fail "postrm fallback contract is missing: $contract"
done

if grep -Eiq 'sed[[:space:]]+-i|(^|[[:space:]])(cp|mv|ln)[[:space:]].*(themes/bootstrap|luci-static/bootstrap)' "$DEFAULT" "$MAKEFILE"; then
	fail 'theme mutates Bootstrap-owned files'
fi

echo 'ProxyPool theme source safety: PASS'
