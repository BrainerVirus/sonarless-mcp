#!/usr/bin/env sh
# sonarless-mcp installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --yes        # args go to `sonarless-mcp setup`
#
# Detects OS/CPU, downloads the matching release, verifies its checksum,
# installs to ~/.local/bin, then opens the client picker (`sonarless-mcp setup`).
#
# Env: SONARLESS_MCP_VERSION (tag, default latest), SONARLESS_MCP_INSTALL_DIR,
#      SONARLESS_MCP_BASE_URL (download base; for mirrors and tests),
#      SONARLESS_MCP_NO_SETUP=1 (install only).
set -eu

REPO="BrainerVirus/sonarless-mcp"
INSTALL_DIR="${SONARLESS_MCP_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  MINGW* | MSYS* | CYGWIN*) die "on Windows use PowerShell: irm https://raw.githubusercontent.com/$REPO/main/install.ps1 | iex" ;;
  *) die "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported CPU: $(uname -m)" ;;
esac

if [ -n "${SONARLESS_MCP_BASE_URL:-}" ]; then
  base="$SONARLESS_MCP_BASE_URL"
elif [ -n "${SONARLESS_MCP_VERSION:-}" ]; then
  base="https://github.com/$REPO/releases/download/$SONARLESS_MCP_VERSION"
else
  base="https://github.com/$REPO/releases/latest/download"
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
else
  die "need curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "need sha256sum or shasum to verify the download"
fi

asset="sonarless-mcp_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Detected $os/$arch. Downloading $asset..."
fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

want="$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$want" ] || die "$asset not listed in checksums.txt"
[ "$(sha256 "$tmp/$asset")" = "$want" ] || die "checksum mismatch for $asset"
say "Checksum OK."

tar -xzf "$tmp/$asset" -C "$tmp" sonarless-mcp
mkdir -p "$INSTALL_DIR"
# Replace via rename so a running copy (an MCP client using it) isn't disturbed.
cp "$tmp/sonarless-mcp" "$INSTALL_DIR/.sonarless-mcp.new"
chmod 755 "$INSTALL_DIR/.sonarless-mcp.new"
mv -f "$INSTALL_DIR/.sonarless-mcp.new" "$INSTALL_DIR/sonarless-mcp"
say "Installed $("$INSTALL_DIR/sonarless-mcp" --version) to $INSTALL_DIR/sonarless-mcp"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say "note: $INSTALL_DIR is not on your PATH; add it to your shell profile." ;;
esac
command -v docker >/dev/null 2>&1 || say "note: Docker not found; sonarless-mcp needs Docker (Docker Desktop on macOS) to run SonarQube."

[ "${SONARLESS_MCP_NO_SETUP:-}" = 1 ] && exit 0
say ""
if [ "$#" -gt 0 ]; then
  exec "$INSTALL_DIR/sonarless-mcp" setup "$@"
elif [ -t 1 ] && (: </dev/tty) 2>/dev/null; then
  # stdin is the piped script under `curl | sh`; the picker reads the terminal.
  exec "$INSTALL_DIR/sonarless-mcp" setup </dev/tty
else
  # No terminal (an AI agent or a script): show what was found and how to finish.
  "$INSTALL_DIR/sonarless-mcp" setup --list
  say ""
  say "Next: register it in the clients you want, e.g."
  say "  sonarless-mcp setup --clients claude,cursor    (or --yes for every detected client)"
  say "then restart those clients."
fi
