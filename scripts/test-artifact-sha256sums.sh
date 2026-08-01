#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
REGENERATE="$ROOT/scripts/regenerate-sha256sums.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM
OUTPUT="$TEST_TMP/output"

mkdir -p "$OUTPUT"
printf 'firmware\n' >"$OUTPUT/router-sysupgrade.bin"
printf 'package\n' >"$OUTPUT/proxypool-core.ipk"
printf 'build log\n' >"$OUTPUT/proxypool-core-build.log"
printf 'stale upstream entry\n' >"$OUTPUT/sha256sums"

sh "$REGENERATE" "$OUTPUT"

cut -c67- "$OUTPUT/sha256sums" >"$TEST_TMP/names"
printf '%s\n' \
	'proxypool-core-build.log' \
	'proxypool-core.ipk' \
	'router-sysupgrade.bin' >"$TEST_TMP/expected-names"
cmp -s "$TEST_TMP/expected-names" "$TEST_TMP/names"
(
	cd "$OUTPUT"
	sha256sum -c sha256sums
)
! grep -Fq 'stale upstream entry' "$OUTPUT/sha256sums"

# Regeneration must reflect the current output bytes, not preserve a prior
# manifest merely because one already exists.
printf 'updated firmware\n' >"$OUTPUT/router-sysupgrade.bin"
sh "$REGENERATE" "$OUTPUT"
(
	cd "$OUTPUT"
	sha256sum -c sha256sums
)

mkdir "$TEST_TMP/empty"
if sh "$REGENERATE" "$TEST_TMP/empty"; then
	echo 'empty artifact directory unexpectedly produced a manifest' >&2
	exit 1
fi
[ ! -e "$TEST_TMP/empty/sha256sums" ]

echo 'artifact sha256sums: PASS'
