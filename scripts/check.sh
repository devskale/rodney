#!/bin/bash
# check.sh — local guardrail for the rodney fork (zero GitHub Actions minutes).
#   ./scripts/check.sh          fast: go vet + go build (~seconds)
#   ./scripts/check.sh --full   full: + go test (~98s)
# The pre-push hook runs the fast tier by default; --full / CHECK=1 on demand.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "── go vet ──"
go vet ./...

echo "── go build ──"
go build -o /tmp/rodney-check-bin .
rm -f /tmp/rodney-check-bin
echo "  build ok"

if [ "${1:-}" = "--full" ] || [ "${CHECK:-0}" = "1" ]; then
    echo "── go test (full) ──"
    go test ./...
fi

echo "✓ check ok"
