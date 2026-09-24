#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
case "$(uname -s):$(uname -m)" in
    Linux:x86_64) platform=linux; arch=amd64 ;;
    Linux:aarch64|Linux:arm64) platform=linux; arch=arm64 ;;
    Darwin:x86_64) platform=macos; arch=amd64 ;;
    Darwin:arm64) platform=macos; arch=arm64 ;;
    *) echo "Unsupported Plato LS binary target: $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac

name="plato-ls-$platform-$arch"
mkdir -p dist
CGO_ENABLED=1 go build -mod=readonly -trimpath -buildvcs=false -o "dist/$name" .
"dist/$name" version
(
    cd dist
    if [ "$platform" = macos ]; then
        shasum -a 256 "$name" > SHA256SUMS
        shasum -a 256 -c SHA256SUMS
    else
        sha256sum "$name" > SHA256SUMS
        sha256sum -c SHA256SUMS
    fi
)
