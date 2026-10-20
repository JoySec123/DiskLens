#!/bin/bash
set -eu
ARCH="${1:-amd64}"
case "${ARCH}" in
  amd64) PLATFORM="x86" ;;
  arm64) PLATFORM="arm" ;;
  *) echo "usage: $0 {amd64|arm64}" >&2; exit 2 ;;
esac
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
FNPACK="${FNPACK:-${ROOT}/.tools/fnpack}"
if [ ! -x "${FNPACK}" ]; then FNPACK="$(command -v fnpack)"; fi
mkdir -p "${ROOT}/.build" "${ROOT}/dist"
BUILD_DIR="$(mktemp -d "${ROOT}/.build/fnos-${ARCH}.XXXXXX")"
# Use a fresh staging directory; preserve earlier test packages.
COPYFILE_DISABLE=1 tar --exclude='.DS_Store' --exclude='._*' -cf - -C "${ROOT}/fnos" . | tar -xf - -C "${BUILD_DIR}"
mkdir -p "${BUILD_DIR}/app/bin"
node scripts/build-licenses.mjs "${BUILD_DIR}/app/licenses" "${ARCH}"
GOOS=linux GOARCH="${ARCH}" CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "${BUILD_DIR}/app/bin/disklens" ./cmd/disklens
sed "s/platform=\"x86\"/platform=\"${PLATFORM}\"/" "${ROOT}/fnos/manifest" > "${BUILD_DIR}/manifest"
chmod +x "${BUILD_DIR}"/cmd/*
(cd "${BUILD_DIR}" && "${FNPACK}" build)
# fnpack can return zero even after a validation error.
test -s "${BUILD_DIR}/DiskLens.fpk"
VERSION="$(sed -n 's/^version="\(.*\)"/\1/p' "${ROOT}/fnos/manifest")"
OUTPUT="${ROOT}/dist/DiskLens-${VERSION}-${ARCH}.fpk"
cp "${BUILD_DIR}/DiskLens.fpk" "${OUTPUT}"
shasum -a 256 "${OUTPUT}"
echo "Built ${OUTPUT}"
