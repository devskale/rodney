#!/bin/bash
# Local release builder — replaces GitHub Actions for releases.
# Cross-compiles all platform binaries, tars them, creates the GitHub release
# via gh, uploads the assets. Zero Actions minutes.
#
# Usage: bash scripts/release.sh <version>     # e.g. bash scripts/release.sh 0.6.2
set -euo pipefail

REPO="devskale/rodney"
VERSION="${1:?usage: release.sh <version> (no leading v)}"
TAG="v$VERSION"

cd "$(dirname "$0")/.."

echo "── Building $TAG locally (cross-compile, zero CI minutes) ──"
rm -rf dist && mkdir -p dist

for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
    GOOS="${target%/*}"; GOARCH="${target#*/}"
    out="dist/rodney-$GOOS-$GOARCH"
    mkdir -p "$out"
    echo "  building $GOOS/$GOARCH ..."
    GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
        -o "$out/rodney" .
    tar -C "$out" -czf "dist/rodney-$GOOS-$GOARCH.tar.gz" rodney
done

# sanity: the native binary reports the right version
NATIVE="$(uname -s | tr '[:upper:]' '[:lower:]')/$(uname -m)"
case "$(uname -m)" in x86_64) NATIVE="${NATIVE%/*}/amd64";; aarch64|arm64) NATIVE="${NATIVE%/*}/arm64";; esac
BIN="dist/rodney-${NATIVE/\//-}/rodney"
echo "── Sanity: $BIN --version ──"
"$BIN" --version | grep -Fx "$VERSION" || { echo "version mismatch"; exit 1; }

echo "── Creating release $TAG ──"
if gh release view "$TAG" --repo "$REPO" >/dev/null 2>&1; then
    echo "  release exists — uploading assets to it"
    gh release upload "$TAG" dist/*.tar.gz --repo "$REPO" --clobber
else
    gh release create "$TAG" --repo "$REPO" --title "$TAG" \
        --generate-notes dist/*.tar.gz
fi

echo "── Done: https://github.com/$REPO/releases/tag/$TAG ──"
