#!/bin/bash
# Build frz (Go edition) for macOS and Linux with version info injected.
# Requires: Go toolchain in PATH (https://go.dev/dl/).
#
#   ./build.sh            # build all binaries with VERSION below
#   VERSION=v1.1.0 ./build.sh
set -e

VERSION=${VERSION:-v1.0.0}
BUILD_TIME=$(date +%Y-%m-%d)
# -s -w strips the symbol table and debug info (smaller binaries, harder to disassemble)
LDFLAGS="-s -w -X main.version=$VERSION -X main.buildTime=$BUILD_TIME"

cd "$(dirname "$0")"
go build -ldflags "$LDFLAGS" -o frz-go .
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags "$LDFLAGS" -o frz-linux-amd64 .
CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -ldflags "$LDFLAGS" -o frz-linux-arm64 .
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$LDFLAGS" -o frz-darwin-amd64 .

echo "built $VERSION ($BUILD_TIME):"
ls -lh frz-go frz-linux-amd64 frz-linux-arm64 frz-darwin-amd64 | awk '{print "  " $5, $9}'
