#!/usr/bin/env bash
# Build release archives for every supported platform.
#
#   VERSION=1.2.3 ./build.sh
#
# Output: dist/fastrtmp-<version>-<os>-<arch>.{tar.gz|zip} plus SHA256SUMS.
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-1.1.0}"
LDFLAGS="-s -w -X main.version=${VERSION}"
OUT=dist
rm -rf "$OUT"
mkdir -p "$OUT"

build() {
  local goos="$1" goarch="$2" ext="$3"
  local name="fastrtmp-${VERSION}-${goos}-${goarch}"
  local dir="${OUT}/${name}"

  echo ">> ${name}"
  mkdir -p "$dir"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags="$LDFLAGS" -o "${dir}/fastrtmp${ext}" ./cmd/fastrtmp

  cp README.md LICENSE "$dir/" 2>/dev/null || true
  if [ "$goos" = "linux" ]; then
    mkdir -p "$dir/deploy"
    cp deploy/fastrtmp.service "$dir/deploy/"
  fi

  if [ "$goos" = "windows" ] && command -v zip >/dev/null 2>&1; then
    ( cd "$OUT" && zip -qr "${name}.zip" "${name}" )
  else
    tar -C "$OUT" -czf "${OUT}/${name}.tar.gz" "${name}"
  fi
  rm -rf "$dir"
}

build linux   amd64 ""
build linux   arm64 ""
build windows amd64 ".exe"
build darwin  amd64 ""
build darwin  arm64 ""

if command -v sha256sum >/dev/null 2>&1; then
  ( cd "$OUT" && sha256sum *.tar.gz *.zip > SHA256SUMS 2>/dev/null || true )
elif command -v shasum >/dev/null 2>&1; then
  ( cd "$OUT" && shasum -a 256 *.tar.gz *.zip > SHA256SUMS 2>/dev/null || true )
fi

echo
ls -lh "$OUT"
