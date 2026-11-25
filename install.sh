#!/usr/bin/env sh
# sonarless-mcp installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --yes        # args go to `sonarless-mcp setup`
#
# Detects OS/CPU, downloads the matching release, verifies its checksum (and,
# when cosign is installed, the checksums file's Sigstore signature by this
# repository's release workflow), installs to ~/.local/bin, then opens the
# client picker (`sonarless-mcp setup`). Later upgrades: `sonarless-mcp upgrade`.
#
# Env: SONARLESS_MCP_VERSION (tag, default latest), SONARLESS_MCP_INSTALL_DIR,
#      SONARLESS_MCP_RELEASES_URL, SONARLESS_MCP_RELEASES_API_URL (release
#      download and API roots; for mirrors and tests),
#      SONARLESS_MCP_BASE_URL (one release's download dir; for CI snapshots),
#      SONARLESS_MCP_NO_SETUP=1 (install only).
set -eu

REPO="BrainerVirus/sonarless-mcp"
# Who signs checksums.txt: release.yml on main (semantic-release runs on the
# push to main). internal/update.ReleaseIdentity pins the same identity.
SIGNER_ISSUER="https://token.actions.githubusercontent.com"
SIGNER_IDENTITY='^https://github\.com/BrainerVirus/sonarless-mcp/\.github/workflows/release\.yml@refs/heads/main$'
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

# Name the release before downloading it, so the archive, its checksums and
# the signature check below are about one known tag. If the API can't be
# reached (rate limit, proxy), fall back to releases/latest with the tag
# unknown.
releases_url="${SONARLESS_MCP_RELEASES_URL:-https://github.com/$REPO/releases}"
api_url="${SONARLESS_MCP_RELEASES_API_URL:-https://api.github.com/repos/$REPO}"
tag="${SONARLESS_MCP_VERSION:-}"
if [ -n "${SONARLESS_MCP_BASE_URL:-}" ]; then
  base="$SONARLESS_MCP_BASE_URL"
else
  if [ -z "$tag" ] && fetch "$api_url/releases/latest" "$tmp/latest.json" 2>/dev/null; then
    tag="$(tr ',' '\n' <"$tmp/latest.json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
    printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' || tag=""
  fi
  if [ -n "$tag" ]; then
    base="$releases_url/download/$tag"
  else
    base="$releases_url/latest/download"
  fi
fi

say "Detected $os/$arch. Downloading $asset${tag:+ ($tag)}..."
fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

# Releases up to v0.8.1 were published before signing existed.
pre_signing() { # tag
  v="${1#v}"; v="${v%%-*}"
  major="${v%%.*}"; rest="${v#*.}"; minor="${rest%%.*}"; patch="${rest#*.}"
  case "$major.$minor.$patch" in *[!0-9.]* | .* | *..* | *.) return 1 ;; esac
  [ "$major" -eq 0 ] && { [ "$minor" -lt 8 ] || { [ "$minor" -eq 8 ] && [ "$patch" -le 1 ]; }; }
}
if command -v cosign >/dev/null 2>&1; then
  if fetch "$base/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json" 2>/dev/null; then
    cosign verify-blob \
      --bundle "$tmp/checksums.txt.sigstore.json" \
      --certificate-oidc-issuer "$SIGNER_ISSUER" \
      --certificate-identity-regexp "$SIGNER_IDENTITY" \
      "$tmp/checksums.txt" >/dev/null 2>&1 ||
      die "the signature of checksums.txt does not verify; nothing was installed"
    say "Verified the cosign signature of checksums.txt."
  elif pre_signing "$tag"; then
    say "$tag was published before releases were signed; checking its SHA-256 only."
  else
    die "the release has no signature (checksums.txt.sigstore.json); nothing was installed"
  fi
else
  say "cosign not found; skipping the signature check (the SHA-256 is still checked)."
fi

want="$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$want" ] || die "$asset not listed in checksums.txt"
[ "$(sha256 "$tmp/$asset")" = "$want" ] || die "checksum mismatch for $asset"
say "Checksum OK."

tar -xzf "$tmp/$asset" -C "$tmp" sonarless-mcp
mkdir -p "$INSTALL_DIR"
[ -d "$INSTALL_DIR/sonarless-mcp" ] && die "$INSTALL_DIR/sonarless-mcp is a directory; move it away and rerun"
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
# What makes the new command show up (and highlight as valid) in an already
# open terminal depends on who checks commands. In almost every terminal it's
# the shell (zsh-syntax-highlighting, fish, bash's hash table), refreshed by
# `rehash` / `hash -r`; Warp's own input editor reads PATH only when a tab
# starts. Terminals identify themselves through these variables.
term="this terminal"
case "${TERM_PROGRAM:-}" in
  WarpTerminal) term="Warp" ;;
  ghostty) term="Ghostty" ;;
  iTerm.app) term="iTerm2" ;;
  Apple_Terminal) term="Terminal" ;;
  vscode) term="the VS Code terminal" ;;
  WezTerm) term="WezTerm" ;;
  Hyper) term="Hyper" ;;
  Tabby) term="Tabby" ;;
  *)
    if [ -n "${KITTY_WINDOW_ID:-}" ]; then term="kitty"
    elif [ -n "${ALACRITTY_SOCKET:-}${ALACRITTY_LOG:-}" ]; then term="Alacritty"
    elif [ -n "${KONSOLE_VERSION:-}" ]; then term="Konsole"
    elif [ "${TERMINAL_EMULATOR:-}" = JetBrains-JediTerm ]; then term="the JetBrains terminal"
    elif [ -n "${WT_SESSION:-}" ]; then term="Windows Terminal"
    elif [ -n "${VTE_VERSION:-}" ]; then term="GNOME Terminal"
    fi ;;
esac
case "$(basename "${SHELL:-sh}")" in
  zsh) refresh="run: rehash   (or open a new tab)" ;;
  bash) refresh="run: hash -r   (or open a new tab)" ;;
  fish) refresh="" ;; # fish re-checks PATH by itself
  *) refresh="open a new tab" ;;
esac
[ "$term" = Warp ] && refresh="open a new tab (Warp reads PATH only when a tab starts; the command already works here)"
if [ "$on_path" = 0 ]; then
  now="export PATH=\"$INSTALL_DIR:\$PATH\""
  [ "$(basename "${SHELL:-sh}")" = fish ] && now="fish_add_path $INSTALL_DIR"
  say "To use sonarless-mcp in this terminal now, run:  $now   (new terminals pick it up)"
fi
if [ -n "$refresh" ]; then
  say "If $term doesn't recognize sonarless-mcp yet, $refresh"
fi
command -v docker >/dev/null 2>&1 || say "note: Docker not found; sonarless-mcp needs Docker (Docker Desktop on macOS) to run SonarQube."

[ "${SONARLESS_MCP_NO_SETUP:-}" = 1 ] && exit 0
say ""
rm -rf "$tmp" # exec below replaces this shell, so the EXIT trap won't run
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
