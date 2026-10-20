#!/bin/bash
set -eu
ARCH="${1:-amd64}"
mkdir -p dist
GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -trimpath -o "dist/disklens-linux-${ARCH}" ./cmd/disklens
echo "built dist/disklens-linux-${ARCH}"
