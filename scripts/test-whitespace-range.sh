#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
CHECK="$ROOT/scripts/check-whitespace-range.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

git -C "$TEST_TMP" init -q
git -C "$TEST_TMP" config user.name 'ProxyPool Test'
git -C "$TEST_TMP" config user.email 'proxypool-test.invalid'
printf 'clean\n' >"$TEST_TMP/file.txt"
git -C "$TEST_TMP" add file.txt
git -C "$TEST_TMP" commit -qm base
base=$(git -C "$TEST_TMP" rev-parse HEAD)

printf 'trailing space \n' >>"$TEST_TMP/file.txt"
git -C "$TEST_TMP" add file.txt
git -C "$TEST_TMP" commit -qm bad
bad=$(git -C "$TEST_TMP" rev-parse HEAD)
if (cd "$TEST_TMP" && sh "$CHECK" "$base" "$bad"); then
	echo 'range checker accepted introduced trailing whitespace' >&2
	exit 1
fi

git -C "$TEST_TMP" switch -q -c clean "$base"
printf 'also clean\n' >>"$TEST_TMP/file.txt"
git -C "$TEST_TMP" add file.txt
git -C "$TEST_TMP" commit -qm clean
clean=$(git -C "$TEST_TMP" rev-parse HEAD)
(cd "$TEST_TMP" && sh "$CHECK" "$base" "$clean")

zero=0000000000000000000000000000000000000000
(cd "$TEST_TMP" && sh "$CHECK" "$zero" "$clean")
if (cd "$TEST_TMP" && sh "$CHECK" not-a-commit "$clean"); then
	echo 'range checker accepted an invalid base revision' >&2
	exit 1
fi

echo 'whitespace range behavior: PASS'
