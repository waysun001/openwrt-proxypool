#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
sh "$ROOT/scripts/test-dns-fail-closed.sh"
sh "$ROOT/scripts/test-kernel-isolation-contract.sh"
sh "$ROOT/scripts/test-package-safety-integration.sh"
sh "$ROOT/tests/integration/round3_harness_semantics_test.sh"
echo 'PASS: integrated fail-closed contracts'
