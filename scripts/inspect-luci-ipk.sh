#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
	echo 'usage: inspect-luci-ipk.sh LUCI_PACKAGE.ipk' >&2
	exit 2
fi

package=$1
[ -f "$package" ] || { echo "package not found: $package" >&2; exit 1; }

inspect_tmp=$(mktemp -d)
trap 'rm -rf "$inspect_tmp"' EXIT HUP INT TERM
mkdir "$inspect_tmp/outer" "$inspect_tmp/data" "$inspect_tmp/control"
tar -xzf "$package" -C "$inspect_tmp/outer"
[ "$(tr -d '\r\n' <"$inspect_tmp/outer/debian-binary")" = 2.0 ] || {
	echo 'invalid IPK format version' >&2
	exit 1
}
tar -xzf "$inspect_tmp/outer/data.tar.gz" -C "$inspect_tmp/data"
tar -xzf "$inspect_tmp/outer/control.tar.gz" -C "$inspect_tmp/control"

control="$inspect_tmp/control/control"
grep -Fqx 'Package: luci-app-proxypool' "$control" || {
	echo 'unexpected package name metadata' >&2
	exit 1
}
grep -Fqx 'Architecture: all' "$control" || {
	echo 'unexpected package architecture metadata' >&2
	exit 1
}

dependency_names=$(
	sed -n 's/^Depends:[[:space:]]*//p' "$control" |
		tr ',' '\n' |
		sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*(.*$//' -e 's/[[:space:]]*$//'
)
for required_dependency in proxypool-core luci-base luci-lua-runtime luci-compat luci-lib-jsonc; do
	printf '%s\n' "$dependency_names" | grep -Fqx "$required_dependency" || {
		echo "missing required dependency: $required_dependency" >&2
		exit 1
	}
done

if find "$inspect_tmp/data" -type l -print -quit | grep -q .; then
	echo 'LuCI package payload must not contain symbolic links' >&2
	exit 1
fi

cat >"$inspect_tmp/expected-payload" <<'EOF'
etc/uci-defaults/luci-proxypool
usr/lib/lua/luci/controller/proxypool.lua
usr/lib/lua/luci/view/proxypool/lease.htm
usr/lib/lua/luci/view/proxypool/locked.htm
usr/lib/lua/luci/view/proxypool/main.htm
www/luci-static/resources/proxypool-global.css
www/luci-static/resources/proxypool-global.js
EOF
find "$inspect_tmp/data" -type f -printf '%P\n' | LC_ALL=C sort >"$inspect_tmp/actual-payload"
if ! cmp -s "$inspect_tmp/expected-payload" "$inspect_tmp/actual-payload"; then
	echo 'unexpected LuCI package payload' >&2
	diff -u "$inspect_tmp/expected-payload" "$inspect_tmp/actual-payload" >&2 || true
	exit 1
fi

require_mode() {
	relative=$1
	expected_mode=$2
	file="$inspect_tmp/data/$relative"
	[ -f "$file" ] && [ ! -L "$file" ] || {
		echo "missing regular /$relative" >&2
		exit 1
	}
	[ "$(stat -c '%a' "$file")" = "$expected_mode" ] || {
		echo "unexpected mode for /$relative" >&2
		exit 1
	}
}

require_mode etc/uci-defaults/luci-proxypool 755
for relative in \
	usr/lib/lua/luci/controller/proxypool.lua \
	usr/lib/lua/luci/view/proxypool/lease.htm \
	usr/lib/lua/luci/view/proxypool/locked.htm \
	usr/lib/lua/luci/view/proxypool/main.htm \
	www/luci-static/resources/proxypool-global.css \
	www/luci-static/resources/proxypool-global.js; do
	require_mode "$relative" 644
done

uci_default="$inspect_tmp/data/etc/uci-defaults/luci-proxypool"
if grep -Eiq 'xl2tpd|(^|[^[:alnum:]_])(network|firewall|wireless)\.|wifi([[:space:]]+(config|reload|up))?|encryption=.?(none)|/www/|luci\.js|cascade\.css|header\.htm|/etc/init\.d/|uhttpd|sed[[:space:]]+-i' "$uci_default"; then
	echo 'uci-default contains router provisioning or global LuCI mutation' >&2
	exit 1
fi
for ucitrack_contract in \
	'delete ucitrack.@proxypool[-1]' \
	'add ucitrack proxypool' \
	"set ucitrack.@proxypool[-1].init='proxypool'" \
	'commit ucitrack'; do
	grep -Fqx "$ucitrack_contract" "$uci_default" || {
		echo 'uci-default is missing the safe ucitrack transaction' >&2
		exit 1
	}
done

main_view="$inspect_tmp/data/usr/lib/lua/luci/view/proxypool/main.htm"
[ "$(grep -Ec '<link rel="stylesheet" href="<%=resource%>/proxypool-global\.css(\?v=[A-Za-z0-9._+-]+)?" />' "$main_view")" -eq 1 ] &&
	[ "$(grep -Ec '<script type="text/javascript" src="<%=resource%>/proxypool-global\.js(\?v=[A-Za-z0-9._+-]+)?"></script>' "$main_view")" -eq 1 ] || {
	echo 'main view does not load packaged global assets' >&2
	exit 1
}

for asset in \
	"$inspect_tmp/data/www/luci-static/resources/proxypool-global.css" \
	"$inspect_tmp/data/www/luci-static/resources/proxypool-global.js"; do
	[ -s "$asset" ] || { echo "packaged asset is empty: ${asset##*/}" >&2; exit 1; }
done

echo 'LuCI IPK contents: PASS'
