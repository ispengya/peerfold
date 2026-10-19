#!/usr/bin/env bash
# install.sh — download the peerfold binary from GitHub Releases
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/ispengya/peerfold/main/install.sh | sh
#   ./install.sh              # install the latest release
#   ./install.sh v1.0.0       # install a specific tag
#   ./install.sh --uninstall

set -euo pipefail

REPO="${PEERFOLD_REPO:-ispengya/peerfold}"
INSTALL_DIR="${PEERFOLD_INSTALL_DIR:-$HOME/.local/bin}"
BINARY_NAME="peerfold"

info() { echo "       $1"; }
ok()   { echo "       [OK] $1"; }
fail() { echo "       [X] $1"; }

usage() {
  cat <<EOF
usage: install.sh [VERSION | --uninstall]

  VERSION       release tag to install (default: latest)
  --uninstall   remove the installed binary

environment:
  PEERFOLD_REPO          GitHub repo (default: ${REPO})
  PEERFOLD_INSTALL_DIR   install directory (default: ${INSTALL_DIR})
EOF
}

# ── Args ─────────────────────────────────────────────────────

TAG="${PEERFOLD_VERSION:-latest}"
if [ $# -gt 0 ]; then
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --uninstall)
      rm -f "${INSTALL_DIR}/${BINARY_NAME}"
      ok "Removed ${INSTALL_DIR}/${BINARY_NAME}"
      exit 0
      ;;
    *) TAG="$1" ;;
  esac
fi

# ── Platform ─────────────────────────────────────────────────

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  armv7l)        arch="arm" ;;
  *) fail "unsupported architecture: $arch"; exit 1 ;;
esac

case "$os" in
  linux)  asset="peerfold-linux-${arch}" ;;
  darwin) asset="peerfold-darwin-${arch}" ;;
  *) fail "unsupported OS: $os (use install.ps1 on Windows)"; exit 1 ;;
esac

# ── Resolve release URL ──────────────────────────────────────

if [ "$TAG" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${TAG}/${asset}"
fi

echo ""
echo "  peerfold installer"
echo "  ------------------------------------"
echo ""
info "Repository: ${REPO}"
info "Version:    ${TAG}"
info "Asset:      ${asset}"
info "Install to: ${INSTALL_DIR}"
echo ""

# ── Download ─────────────────────────────────────────────────

mkdir -p "$INSTALL_DIR"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

echo "  Downloading..."
if command -v curl >/dev/null 2>&1; then
  curl -fsSL -o "$tmp" "$url"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$tmp" "$url"
else
  fail "Need curl or wget to download the release."
  exit 1
fi

chmod +x "$tmp"
mv "$tmp" "${INSTALL_DIR}/${BINARY_NAME}"
ok "Installed ${INSTALL_DIR}/${BINARY_NAME}"

# ── PATH ─────────────────────────────────────────────────────

case ":$PATH:" in
  *":${INSTALL_DIR}:"*)
    ok "Already in PATH: ${INSTALL_DIR}"
    ;;
  *)
    rc="$HOME/.bashrc"
    [ -n "${ZSH_VERSION:-}" ] && rc="$HOME/.zshrc"
    marker="# peerfold"
    if ! grep -qF "$marker" "$rc" 2>/dev/null; then
      {
        echo ""
        echo "$marker"
        echo "export PATH=\"\$PATH:${INSTALL_DIR}\""
      } >> "$rc"
    fi
    ok "Added to PATH in $rc"
    info "Run: export PATH=\"\$PATH:${INSTALL_DIR}\"  (or open a new terminal)"
    ;;
esac

# ── FUSE hint ────────────────────────────────────────────────

if [ ! -e /dev/fuse ] && ! command -v fusermount3 >/dev/null 2>&1 && ! command -v fusermount >/dev/null 2>&1; then
  echo ""
  info "FUSE is not installed. 'peerfold mount' needs it:"
  info "  Debian/Ubuntu:  sudo apt install fuse3"
  info "  Fedora:         sudo dnf install fuse3"
  info "  Arch:           sudo pacman -S fuse3"
fi

echo ""
echo "  ------------------------------------"
echo "  Done! Next:"
echo ""
echo "    peerfold server     # start the aggregation server"
echo "    peerfold client     # share local directories"
echo "    peerfold mount      # mount the federated tree"
echo ""
