#!/usr/bin/env bash
set -euo pipefail

# 在本地用 protoc + 本地插件生成 gRPC / gRPC-Gateway 代码。
# 依赖（一次性安装）：
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
#   go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@v2.22.0

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
export PATH="$(go env GOPATH)/bin:$PATH"

PROTO_DIR="pb/proto"
OUT="pb/gen"
mkdir -p "$OUT"

protoc \
  -I "$PROTO_DIR" \
  -I pb/third_party \
  --go_out="$OUT" --go_opt=paths=source_relative \
  --go-grpc_out="$OUT" --go-grpc_opt=paths=source_relative \
  --grpc-gateway_out="$OUT" --grpc-gateway_opt=paths=source_relative \
  $(find "$PROTO_DIR" -name '*.proto')

echo "proto generated -> $OUT"
