#!/usr/bin/env bash
# Regenerates the Operational Data Source gRPC stubs.
# Requires: protoc, protoc-gen-go, protoc-gen-go-grpc on PATH.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT
protoc --proto_path="$ROOT/api/proto" --go_out="$OUT" --go-grpc_out="$OUT" operational/v1/operational.proto
cp "$OUT"/github.com/udaykishore/ttl-aware-bff/internal/datasource/operational/opsv1/*.go \
   "$ROOT/internal/datasource/operational/opsv1/"
echo "generated -> internal/datasource/operational/opsv1"
