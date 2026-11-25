#!/bin/sh
# Signature matrix for install.sh: a stub cosign on PATH, file:// fixtures
# built from a snapshot release, no network. CI runs it after the snapshot:
#
#   sh .github/scripts/test-install-signature.sh dist
#
# A row names the release "latest" resolves to (or the pinned tag), whether it
# carries checksums.txt.sigstore.json, what the stub cosign answers, and
# whether install.sh must install. test-install-signature.ps1 is the same
# matrix for install.ps1.
set -eu

root="$(cd "$(dirname "$0")/../.." && pwd)"
dist="$(cd "${1:?usage: test-install-signature.sh <snapshot dist dir>}" && pwd)"
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 2 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported CPU: $(uname -m)" >&2; exit 2 ;;
esac
asset="sonarless-mcp_${os}_${arch}.tar.gz"

w="$(mktemp -d)"
trap 'rm -rf "$w"' EXIT INT TERM

# Releases: v0.8.1 (before signing, no bundle), v0.9.0 (unsigned, must be
# refused), v0.9.1 (with a bundle); releases/latest/download serves v0.9.0.
for tag in v0.8.1 v0.9.0 v0.9.1; do
  mkdir -p "$w/releases/download/$tag"
  cp "$dist/$asset" "$dist/checksums.txt" "$w/releases/download/$tag/"
done
echo '{}' >"$w/releases/download/v0.9.1/checksums.txt.sigstore.json"
mkdir -p "$w/releases/latest"
cp -R "$w/releases/download/v0.9.0" "$w/releases/latest/download"
for tag in v0.8.1 v0.9.0 v0.9.1; do
  mkdir -p "$w/api-$tag/releases"
  printf '{"url":"x","tag_name":"%s","name":"%s"}' "$tag" "$tag" >"$w/api-$tag/releases/latest"
done
mkdir -p "$w/api-down" "$w/cosign-ok" "$w/cosign-bad"
printf '#!/bin/sh\nexit 0\n' >"$w/cosign-ok/cosign"
printf '#!/bin/sh\nexit 1\n' >"$w/cosign-bad/cosign"
chmod +x "$w/cosign-ok/cosign" "$w/cosign-bad/cosign"

failed=0
row() { # name want(yes|no) cosign(none|ok|bad) api pinned-tag [base-url]
  if [ "$3" = none ]; then
    if command -v cosign >/dev/null 2>&1; then
      echo "skip $1 (cosign is installed on this machine)"
      return 0
    fi
    path="$PATH"
  else
    path="$w/cosign-$3:$PATH"
  fi
  rc=0
  out="$(env PATH="$path" \
    SONARLESS_MCP_RELEASES_URL="file://$w/releases" \
    SONARLESS_MCP_RELEASES_API_URL="file://$w/$4" \
    SONARLESS_MCP_VERSION="$5" SONARLESS_MCP_BASE_URL="${6:-}" \
    SONARLESS_MCP_INSTALL_DIR="$w/bin" SONARLESS_MCP_NO_SETUP=1 SONARLESS_MCP_NO_MODIFY_PATH=1 \
    sh "$root/install.sh" 2>&1)" || rc=$?
  got=no
  [ "$rc" = 0 ] && [ -x "$w/bin/sonarless-mcp" ] && got=yes
  if [ "$got" = "$2" ]; then
    echo "ok   $1"
  else
    echo "FAIL $1: installed=$got (exit $rc), want $2"
    printf '%s\n' "$out" | sed 's/^/     /'
    failed=1
  fi
  rm -rf "${w:?}/bin"
}

row "no cosign, latest is v0.8.1" yes none api-v0.8.1 ""
row "no cosign, API down (releases/latest)" yes none api-down ""
row "no cosign, base URL (CI snapshot)" yes none api-down "" "file://$w/releases/download/v0.9.0"
row "unsigned, latest is v0.8.1 (before signing)" yes ok api-v0.8.1 ""
row "unsigned, latest is v0.9.0" no ok api-v0.9.0 ""
row "unsigned, API down (tag unknown)" no ok api-down ""
row "unsigned, pinned v0.8.1" yes ok api-v0.9.0 v0.8.1
row "unsigned, pinned v0.9.0" no ok api-v0.8.1 v0.9.0
row "signed, latest v0.9.1, signature fails" no bad api-v0.9.1 ""
row "signed, latest v0.9.1, signature verifies" yes ok api-v0.9.1 ""
row "signed, pinned v0.9.1, signature verifies" yes ok api-v0.8.1 v0.9.1
row "signed, pinned v0.9.1, signature fails" no bad api-v0.8.1 v0.9.1

exit "$failed"
