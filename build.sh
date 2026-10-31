#!/bin/sh
# Runs the tests and builds dist/budget-guard.so for the official CLIProxyAPI image
# (Debian bookworm, linux/amd64) inside golang:1.26-bookworm.
#
# Usage: ./build.sh [PLUGIN_DIR]
#   PLUGIN_DIR  optional: install the plugin there, e.g. <cpa>/plugins/linux/amd64
#               (restart CLIProxyAPI afterwards to load it)
set -eu
cd "$(dirname "$0")"
mkdir -p .cache
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/src/.cache/go-build -e GOMODCACHE=/src/.cache/mod \
  -v "$PWD":/src -w /src golang:1.26-bookworm sh -c '
    go mod tidy >/dev/null && go vet ./... && go test ./guard/ &&
    CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/budget-guard.so . && rm -f dist/budget-guard.h'
ls -l dist/budget-guard.so
if [ -n "${1:-}" ]; then
  mkdir -p "$1"
  install -m 0644 dist/budget-guard.so "$1/budget-guard.so"
  echo "installed to $1 (restart CLIProxyAPI to load)"
fi
