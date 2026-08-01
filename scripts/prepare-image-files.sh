#!/usr/bin/env sh
set -eu

if [ "$#" -ne 2 ]; then
	echo 'usage: prepare-image-files.sh SOURCE DESTINATION' >&2
	exit 2
fi

source_dir=$1
destination_dir=$2

[ -d "$source_dir" ] || {
	printf 'source directory does not exist: %s\n' "$source_dir" >&2
	exit 1
}
[ ! -e "$destination_dir" ] || {
	printf 'destination already exists: %s\n' "$destination_dir" >&2
	exit 1
}

for config_name in proxypool proxypool_v2 proxypool_runtime; do
	source_config="$source_dir/etc/config/$config_name"
	[ -f "$source_config" ] && [ ! -L "$source_config" ] || {
		printf 'source ProxyPool config is missing or not regular: %s\n' "$source_config" >&2
		exit 1
	}
done

mkdir -p "$destination_dir"
cp -a "$source_dir/." "$destination_dir/"

for config_name in proxypool proxypool_v2 proxypool_runtime; do
	config_file="$destination_dir/etc/config/$config_name"
	[ -f "$config_file" ] || {
		printf 'staged ProxyPool config is missing: %s\n' "$config_file" >&2
		exit 1
	}
	chmod 600 "$config_file"
done
