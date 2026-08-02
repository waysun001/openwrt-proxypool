#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
CONTROLLER="$ROOT/luci-app-proxypool/luasrc/controller/proxypool.lua"
UI="$ROOT/luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js"
ROUND3="$ROOT/scripts/device-test/round3-bulk-recovery.sh"
VALID="$ROOT/scripts/device-test/fixtures/import-60-valid.txt"

reject() { grep -E "$2" "$1" >/dev/null 2>&1 && { echo "FAIL: $3" >&2; exit 1; } || true; }
reject "$CONTROLLER" 'os\.execute|sys\.exec|setsid|uci:set|nixio\.socket|/usr/lib/proxypool/' 'LuCI controller contains a direct mutation primitive'
reject "$UI" 'sequentialConnect|pollJob|pending marker' 'V2 UI contains a browser-driven job loop'
grep -F 'post("api_write")' "$CONTROLLER" >/dev/null || { echo 'FAIL: LuCI writes are not POST protected' >&2; exit 1; }
grep -F 'post("diagnostics_download")' "$CONTROLLER" >/dev/null || { echo 'FAIL: diagnostic download is not POST protected' >&2; exit 1; }
[ "$(wc -l <"$VALID" | tr -d ' ')" -eq 60 ] || { echo 'FAIL: 60-node fixture has the wrong size' >&2; exit 1; }
sh -n "$ROUND3"
sh "$ROOT/scripts/test-round3-hardware-contract.sh"

GOFMT_FILES=$(gofmt -l "$ROOT/proxypool-core/src/proxypoold")
[ -z "$GOFMT_FILES" ] || { printf 'FAIL: gofmt required:\n%s\n' "$GOFMT_FILES" >&2; exit 1; }
echo 'PASS: V2 Phase 5 static and bulk-test gates'
