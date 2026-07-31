#!/usr/bin/env sh
set -eu

cd "$(dirname "$0")/../proxypool-core/src/proxypoold"

go test ./...
go vet ./...

echo "SKIP: Lua test suite does not exist yet"
echo "SKIP: Node test suite does not exist yet"
