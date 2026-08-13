#!/usr/bin/env bash
# Build the Go sidecar binary the plugin talks to.
set -euo pipefail
cd "$(dirname "$0")/sidecar"
go build -o ../bin/hx-go-tool .
echo "built $(cd .. && pwd)/bin/hx-go-tool"
