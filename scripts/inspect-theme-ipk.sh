#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
	echo 'usage: inspect-theme-ipk.sh THEME_PACKAGE.ipk' >&2
	exit 2
fi

package=$1
[ -f "$package" ] || { echo "package not found: $package" >&2; exit 1; }

inspect_tmp=$(mktemp -d)
trap 'rm -rf "$inspect_tmp"' EXIT HUP INT TERM
mkdir "$inspect_tmp/outer" "$inspect_tmp/data" "$inspect_tmp/control"
tar --same-permissions -xzf "$package" -C "$inspect_tmp/outer"
[ "$(tr -d '\r\n' <"$inspect_tmp/outer/debian-binary")" = 2.0 ] || {
	echo 'invalid IPK format version' >&2
	exit 1
}
tar --same-permissions -xzf "$inspect_tmp/outer/data.tar.gz" -C "$inspect_tmp/data"
tar --same-permissions -xzf "$inspect_tmp/outer/control.tar.gz" -C "$inspect_tmp/control"

control="$inspect_tmp/control/control"
grep -Fqx 'Package: luci-theme-proxypool' "$control" || { echo 'unexpected theme package name metadata' >&2; exit 1; }
grep -Fqx 'Architecture: all' "$control" || { echo 'unexpected theme package architecture metadata' >&2; exit 1; }
dependency_names=$(sed -n 's/^Depends:[[:space:]]*//p' "$control" | tr ',' '\n' |
	sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*(.*$//' -e 's/[[:space:]]*$//')
for required_dependency in luci-base luci-theme-bootstrap; do
	printf '%s\n' "$dependency_names" | grep -Fqx "$required_dependency" || {
		echo "missing required theme dependency: $required_dependency" >&2
		exit 1
	}
done

if find "$inspect_tmp/data" "$inspect_tmp/control" -type l -print -quit | grep -q .; then
	echo 'theme package must not contain symbolic links' >&2
	exit 1
fi

cat >"$inspect_tmp/expected-payload" <<'EOF'
etc/uci-defaults/30_luci-theme-proxypool
usr/share/ucode/luci/template/themes/proxypool/footer.ut
usr/share/ucode/luci/template/themes/proxypool/header.ut
usr/share/ucode/luci/template/themes/proxypool/sysauth.ut
www/luci-static/proxypool/proxypool-global.css
www/luci-static/proxypool/proxypool-global.js
EOF
find "$inspect_tmp/data" -type f -printf '%P\n' | LC_ALL=C sort >"$inspect_tmp/actual-payload"
if ! cmp -s "$inspect_tmp/expected-payload" "$inspect_tmp/actual-payload"; then
	echo 'unexpected theme package payload' >&2
	diff -u "$inspect_tmp/expected-payload" "$inspect_tmp/actual-payload" >&2 || true
	exit 1
fi

require_mode() {
	relative=$1
	expected=$2
	file="$inspect_tmp/data/$relative"
	[ -f "$file" ] && [ ! -L "$file" ] || { echo "missing regular /$relative" >&2; exit 1; }
	[ "$(stat -c '%a' "$file")" = "$expected" ] || { echo "unexpected mode for /$relative" >&2; exit 1; }
}
require_mode etc/uci-defaults/30_luci-theme-proxypool 755
for relative in \
	usr/share/ucode/luci/template/themes/proxypool/footer.ut \
	usr/share/ucode/luci/template/themes/proxypool/header.ut \
	usr/share/ucode/luci/template/themes/proxypool/sysauth.ut \
	www/luci-static/proxypool/proxypool-global.css \
	www/luci-static/proxypool/proxypool-global.js; do
	require_mode "$relative" 644
done

default="$inspect_tmp/data/etc/uci-defaults/30_luci-theme-proxypool"
for contract in \
	"luci.themes.ProxyPool='/luci-static/proxypool'" \
	"luci.main.mediaurlbase='/luci-static/proxypool'" \
	'header.ut' 'footer.ut' 'proxypool-global.css' 'proxypool-global.js'; do
	grep -Fq "$contract" "$default" || { echo "theme activation contract missing: $contract" >&2; exit 1; }
done

postrm="$inspect_tmp/control/postrm"
[ -f "$postrm" ] && [ ! -L "$postrm" ] || { echo 'theme postrm is missing' >&2; exit 1; }
[ "$(stat -c '%a' "$postrm")" = 755 ] || { echo 'unexpected mode for theme postrm' >&2; exit 1; }
for contract in '/luci-static/proxypool' '/luci-static/bootstrap' \
	'uci -q delete luci.themes.ProxyPool' 'uci -q commit luci' \
	'/tmp/luci-indexcache' '/tmp/luci-modulecache'; do
	grep -Fq "$contract" "$postrm" || { echo "theme postrm contract missing: $contract" >&2; exit 1; }
done

if find "$inspect_tmp/data" -path '*/themes/bootstrap/*' -o -path '*/luci-static/bootstrap/*' | grep -q .; then
	echo 'theme package owns Bootstrap paths' >&2
	exit 1
fi
if grep -Eiq 'sed[[:space:]]+-i|(^|[[:space:]])(cp|mv|ln)[[:space:]].*(themes/bootstrap|luci-static/bootstrap)' "$default" "$postrm"; then
	echo 'theme package mutates Bootstrap-owned files' >&2
	exit 1
fi

echo 'ProxyPool theme IPK contents: PASS'
