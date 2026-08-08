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
source_activation_request="$source_dir/etc/proxypool/v2-activation-request"
[ -f "$source_activation_request" ] && [ ! -L "$source_activation_request" ] &&
	[ "$(cat "$source_activation_request")" = image ] || {
	printf 'source V2 activation request is missing or invalid: %s\n' "$source_activation_request" >&2
	exit 1
}
source_image_authorization="$source_dir/usr/lib/proxypool/v2-image-activation-authority"
[ -f "$source_image_authorization" ] && [ ! -L "$source_image_authorization" ] &&
	[ "$(cat "$source_image_authorization")" = v2-image-activation-v1 ] || {
	printf 'source full-image activation authorization is missing or invalid: %s\n' "$source_image_authorization" >&2
	exit 1
}

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

state_dir="$destination_dir/etc/proxypool"
activation_request="$state_dir/v2-activation-request"
[ -d "$state_dir" ] && [ ! -L "$state_dir" ] || {
	printf 'staged ProxyPool state directory is missing or unsafe: %s\n' "$state_dir" >&2
	exit 1
}
[ -f "$activation_request" ] && [ ! -L "$activation_request" ] || {
	printf 'staged V2 activation request is missing or unsafe: %s\n' "$activation_request" >&2
	exit 1
}
chmod 700 "$state_dir"
chmod 600 "$activation_request"

image_authorization="$destination_dir/usr/lib/proxypool/v2-image-activation-authority"
[ -f "$image_authorization" ] && [ ! -L "$image_authorization" ] || {
	printf 'staged full-image activation authorization is missing or unsafe: %s\n' "$image_authorization" >&2
	exit 1
}
chmod 644 "$image_authorization"
