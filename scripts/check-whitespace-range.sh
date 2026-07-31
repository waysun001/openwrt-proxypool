#!/usr/bin/env sh
set -eu

if [ "$#" -ne 2 ]; then
	echo 'usage: check-whitespace-range.sh BASE_SHA HEAD_SHA' >&2
	exit 2
fi

base=$1
head=$2
zero=0000000000000000000000000000000000000000

valid_sha() {
	[ "${#1}" -eq 40 ] || return 1
	case "$1" in
		*[!0-9a-fA-F]*) return 1 ;;
	esac
}

valid_sha "$head" || { echo 'invalid head revision' >&2; exit 2; }
git rev-parse --verify --quiet "$head^{commit}" >/dev/null || { echo 'head revision is unavailable' >&2; exit 2; }

if [ "$base" = "$zero" ]; then
	base=$(git hash-object -t tree /dev/null)
else
	valid_sha "$base" || { echo 'invalid base revision' >&2; exit 2; }
	git rev-parse --verify --quiet "$base^{commit}" >/dev/null || { echo 'base revision is unavailable' >&2; exit 2; }
fi

git diff --check "$base" "$head"
