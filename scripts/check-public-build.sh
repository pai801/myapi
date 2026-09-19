#!/usr/bin/env bash
# check-public-build.sh — 本仓门禁：可构建 + 可测试。
#
# 供 CI job 直接调用：任一步失败即 exit 1（CI 变红）。
# 只读检查：不修改任何文件（含 go.mod / go.sum）。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

echo "==> [1/3] go build ./..."
go build ./...

echo "==> [2/3] go vet ./..."
go vet ./...

echo "==> [3/3] go test ./relay/... ./controller/... ./router/... ./middleware/..."
go test ./relay/... ./controller/... ./router/... ./middleware/...
