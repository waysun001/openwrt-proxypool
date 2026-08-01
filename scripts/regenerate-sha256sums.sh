#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
	echo 'usage: regenerate-sha256sums.sh OUTPUT_DIRECTORY' >&2
	exit 2
fi

output_dir=$(CDPATH= cd -- "$1" 2>/dev/null && pwd) || {
	printf 'artifact directory does not exist: %s\n' "$1" >&2
	exit 1
}
list_name=".sha256sums.list.$$"
manifest_name=".sha256sums.manifest.$$"
list_file="$output_dir/$list_name"
manifest_file="$output_dir/$manifest_name"
trap 'rm -f "$list_file" "$manifest_file"' EXIT HUP INT TERM

(
	cd "$output_dir"
	find . -type f \
		! -path './sha256sums' \
		! -name '.sha256sums.*' \
		-print | LC_ALL=C sort >"$list_name"
	[ -s "$list_name" ] || {
		echo 'artifact directory contains no files to hash' >&2
		exit 1
	}
	: >"$manifest_name"
	while IFS= read -r artifact; do
		sha256sum "${artifact#./}" >>"$manifest_name"
	done <"$list_name"
)

mv -f "$manifest_file" "$output_dir/sha256sums"
rm -f "$list_file"
trap - EXIT HUP INT TERM
