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

# Make the command findable, the way rustup/uv do it: if the install dir
# isn't on PATH, add a guarded line to the startup file of every shell found
# on this system (once, marked; SONARLESS_MCP_NO_MODIFY_PATH=1 opts out). A
# child process can't change the calling shell, so also print the command
# that refreshes the current terminal.
marker="# added by sonarless-mcp installer"
posix_line="case \":\$PATH:\" in *\":$INSTALL_DIR:\"*) ;; *) export PATH=\"$INSTALL_DIR:\$PATH\" ;; esac $marker"
added=""
add_line() { # file line
  grep -qsF "$marker" "$1" && return 0
  mkdir -p "$(dirname "$1")"
  printf '\n%s\n' "$2" >> "$1"
  added="$added $1"
}
on_path=1
case ":$PATH:" in *":$INSTALL_DIR:"*) ;; *) on_path=0 ;; esac
if [ "$on_path" = 0 ] && [ "${SONARLESS_MCP_NO_MODIFY_PATH:-}" != 1 ]; then
  add_line "$HOME/.profile" "$posix_line" # login shells, sh/dash
  if command -v bash >/dev/null 2>&1; then
    add_line "$HOME/.bashrc" "$posix_line"
    # macOS bash login shells read .bash_profile instead of .profile when it exists.
    [ "$os" = darwin ] && [ -f "$HOME/.bash_profile" ] && add_line "$HOME/.bash_profile" "$posix_line"
  fi
  # zsh never reads .profile; .zshenv is read by every zsh (login or not).
  command -v zsh >/dev/null 2>&1 && add_line "${ZDOTDIR:-$HOME}/.zshenv" "$posix_line"
  command -v fish >/dev/null 2>&1 && add_line "${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/sonarless-mcp.fish" "fish_add_path $INSTALL_DIR $marker"
  if [ -n "$added" ]; then
    say "Added $INSTALL_DIR to PATH for your shells in:"
    for f in $added; do say "  $f"; done
  fi
fi
case "$(basename "${SHELL:-sh}")" in
  zsh) refresh="rehash" ;;
  bash) refresh="hash -r" ;;
  fish) refresh="" ;;
  *) refresh="" ;;
esac
if [ "$on_path" = 0 ]; then
  now="export PATH=\"$INSTALL_DIR:\$PATH\""
  [ "$(basename "${SHELL:-sh}")" = fish ] && now="fish_add_path $INSTALL_DIR"
  say "To use sonarless-mcp in this terminal now, run:  $now   (new terminals pick it up)"
elif [ -n "$refresh" ]; then
  say "If this terminal doesn't find sonarless-mcp yet, run:  $refresh   (terminals with their own command check, like Warp, need a new tab)"
fi
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
