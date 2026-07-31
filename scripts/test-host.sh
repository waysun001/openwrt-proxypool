#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$ROOT/proxypool-core/src/proxypoold"

case "$(uname -s)" in
	Linux*)
		go test -race -count=1 ./...
		;;
	*)
		go test -count=1 ./...
		echo "SKIP: Go race detector requires a supported Linux host"
		;;
esac
go vet ./...

cd "$ROOT"
for shell_file in \
	proxypool-core/files/proxypool.init \
	scripts/check-whitespace-range.sh \
	scripts/inspect-ipk.sh \
	scripts/test-proxypool-init.sh \
	scripts/test-release-contracts.sh \
	scripts/test-whitespace-range.sh; do
	sh -n "$shell_file"
done
sh scripts/test-proxypool-init.sh
sh scripts/test-release-contracts.sh
sh scripts/test-whitespace-range.sh
git diff --check

echo "SKIP: Lua test suite does not exist yet"
echo "SKIP: Node test suite does not exist yet"
