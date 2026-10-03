#!/usr/bin/env bash
#
# release.sh - cross-compile guest-run for every supported platform, package
# one archive per platform, and optionally publish a GitHub release.
#
# Local-first on purpose: this org runs with GitHub Actions billing off, so the
# build happens here, on your machine, not in CI. Needs `go`, and `gh` only for
# --publish.
#
#   ./release.sh            build + package into dist/
#   ./release.sh --publish  the above, then create the GitHub release
#
set -euo pipefail
cd "$(dirname "$0")"

BIN=guest-run
DIST=dist

# The version lives in exactly one place: the `version` const in main.go.
VERSION=$(grep -E '^const version = ' main.go | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1)
[ -n "${VERSION}" ] || { echo "release.sh: could not read version from main.go" >&2; exit 1; }

# os/arch pairs we ship.
PLATFORMS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"

sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

rm -rf "${DIST}"
mkdir -p "${DIST}"

for p in ${PLATFORMS}; do
  os=${p%/*}; arch=${p#*/}
  name="${BIN}_${VERSION}_${os}_${arch}"
  stage="${DIST}/${name}"
  ext=""; [ "${os}" = "windows" ] && ext=".exe"

  echo ">> building ${os}/${arch}"
  mkdir -p "${stage}"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" \
    go build -trimpath -ldflags="-s -w" -o "${stage}/${BIN}${ext}" .
  cp README.md LICENSE "${stage}/"

  if [ "${os}" = "windows" ]; then
    ( cd "${DIST}" && zip -qr "${name}.zip" "${name}" )
  else
    tar -C "${DIST}" -czf "${DIST}/${name}.tar.gz" "${name}"
  fi
  rm -rf "${stage}"
done

( cd "${DIST}" && sha256 ./*.tar.gz ./*.zip > SHA256SUMS )

echo
echo "Packages for v${VERSION} in ${DIST}/:"
ls -1 "${DIST}"

if [ "${1:-}" = "--publish" ]; then
  command -v gh >/dev/null 2>&1 || { echo "release.sh: gh not found, cannot publish" >&2; exit 1; }
  echo
  echo ">> publishing GitHub release v${VERSION}"
  gh release delete "v${VERSION}" --yes --cleanup-tag 2>/dev/null || true
  gh release create "v${VERSION}" \
    --title "v${VERSION}" \
    --generate-notes \
    "${DIST}"/*.tar.gz "${DIST}"/*.zip "${DIST}/SHA256SUMS"
  echo "done."
fi
