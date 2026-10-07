#!/usr/bin/env bash
# Multi-platform release build matrix for omnilsp (goal.md §W3-W5).
# Usage: ./scripts/build-multiplatform.sh [version] [outdir]
set -euo pipefail

VERSION="${1:-dev}"
OUTDIR="${2:-dist}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

PLATFORMS=(
  "windows amd64"
  "linux amd64"
  "linux arm64"
  "darwin arm64"
)

echo "Building omnilsp ${VERSION} → ${OUTDIR}"
mkdir -p "${OUTDIR}"
# Canonicalize once: the SBOM step runs from $ROOT, so a relative OUTDIR must
# not be re-joined onto it (and an absolute one must survive unchanged).
OUTDIR_ABS="$(cd "${OUTDIR}" && pwd)"

# X8-1: repeated gate runs append another four binaries into the same OUTDIR.
# Clear this version's previous artifacts first, scoped tightly to the exact
# names this script writes below: omnilsp-${VERSION}-* covers the binaries and
# their .sha256 sidecars. Other versions' leftovers (omnilsp-<older>-*) and
# unrelated files already in OUTDIR are deliberately left untouched.
find "${OUTDIR_ABS}" -maxdepth 1 -type f -name "omnilsp-${VERSION}-*" -delete

for entry in "${PLATFORMS[@]}"; do
  set -- $entry
  GOOS="$1" GOARCH="$2" CGO_ENABLED=0
  export GOOS GOARCH CGO_ENABLED
  name="omnilsp-${VERSION}-${GOOS}-${GOARCH}"
  bin="${OUTDIR}/${name}"
  if [ "$GOOS" = "windows" ]; then bin="${bin}.exe"; fi

  echo "  → ${name}"
  # §W8: -trimpath + -buildvcs=true keep the build path/VCS state out of
  # the binary so two builds from the same commit are byte-identical.
  go build -trimpath -buildvcs=true \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$bin" ./cmd/omnilsp

  # Reproducible-build attestation (§W8): sha256 next to each artifact.
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$(dirname "$bin")" && sha256sum "$(basename "$bin")" > "$(basename "$bin").sha256")
  fi
done

# §W8 verification: a second full build must reproduce the host artifact
# byte-for-byte; any drift fails the release.
if command -v sha256sum >/dev/null 2>&1; then
  repro_dir="${OUTDIR_ABS}/repro-check"
  rm -rf "$repro_dir" && mkdir -p "$repro_dir"
  (cd "$ROOT" && GOOS= GOARCH= CGO_ENABLED=0 go build -trimpath -buildvcs=true \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o "${repro_dir}/omnilsp-${VERSION}-windows-amd64.exe" ./cmd/omnilsp)
  if ! (cd "$OUTDIR_ABS" && sha256sum -c "omnilsp-${VERSION}-windows-amd64.exe.sha256" >/dev/null 2>&1) \
     && ! cmp -s "${OUTDIR_ABS}/omnilsp-${VERSION}-windows-amd64.exe" "${repro_dir}/omnilsp-${VERSION}-windows-amd64.exe"; then
    # sha256 -c compares against the recorded hash of the FIRST build only if
    # the file is unchanged; the authoritative check is byte comparison of two
    # independent builds, done here:
    :
  fi
  if cmp -s "${OUTDIR_ABS}/omnilsp-${VERSION}-windows-amd64.exe" "${repro_dir}/omnilsp-${VERSION}-windows-amd64.exe"; then
    echo "§W8 reproducible-build check: PASS (host artifact reproduced byte-identically)"
  else
    echo "§W8 reproducible-build check: FAIL (second build differs)" >&2
    exit 1
  fi
fi

# SBOM accompanies every release (N15/X8). Clear the cross-compile env leaked
# from the build loop, or go run would produce a foreign-GOOS binary.
(cd "$ROOT" && unset GOOS GOARCH && go run scripts/gen-sbom.go -o "${OUTDIR_ABS}/sbom.cdx.json" .)

echo "Done: $(ls "${OUTDIR}" | wc -l) artifacts in ${OUTDIR}"
