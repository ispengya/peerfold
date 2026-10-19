#!/usr/bin/env bash
# setup-client.sh — peerfold client one-click setup (Linux / macOS)
# Installs Go + FUSE when possible, builds the binary, prepares config dirs.
# Mirrors setup-client.ps1.
#
# Usage:
#   ./setup-client.sh
#   ./setup-client.sh --name alice --share-dir /home/alice/docs
#   ./setup-client.sh --rebuild

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_PATH="${PROJECT_ROOT}/peerfold"
HOME_DIR="${HOME:-$(eval echo ~)}"
TOTAL=5

NAME=""
SHARE_DIR=""
FORCE_REBUILD=""

while [ $# -gt 0 ]; do
  case "$1" in
    --name)      NAME="${2:-}"; shift 2 ;;
    --share-dir) SHARE_DIR="${2:-}"; shift 2 ;;
    --rebuild)   FORCE_REBUILD=1; shift ;;
    -h|--help)
      echo "usage: $0 [--name CLIENT] [--share-dir DIR] [--rebuild]"
      exit 0
      ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

# ── Helpers ──────────────────────────────────────────────────

header() {
  echo ""
  echo "  peerfold"
  echo "  Federated filesystem — client setup"
  echo "  ------------------------------------"
  echo ""
}

section() { echo ""; echo "  [$1/$TOTAL] $2"; }
ok()   { echo "       [OK] $1"; }
info() { echo "       $1"; }
warn() { echo "       [!] $1"; }
fail() { echo "       [X] $1"; }

detect_pm() {
  if command -v apt-get >/dev/null 2>&1; then echo apt
  elif command -v dnf >/dev/null 2>&1; then echo dnf
  elif command -v yum >/dev/null 2>&1; then echo yum
  elif command -v pacman >/dev/null 2>&1; then echo pacman
  elif command -v zypper >/dev/null 2>&1; then echo zypper
  elif command -v brew >/dev/null 2>&1; then echo brew
  else echo none
  fi
}

need_root() {
  if [ "$(id -u)" -ne 0 ]; then
    echo "       [!] Root needed to install packages. Re-run with sudo, or install manually."
    return 1
  fi
  return 0
}

install_go() {
  local pm
  pm="$(detect_pm)"
  case "$pm" in
    apt)    need_root && apt-get update -qq && apt-get install -y -qq golang-go ;;
    dnf)    need_root && dnf install -y golang ;;
    yum)    need_root && yum install -y golang ;;
    pacman) need_root && pacman -S --noconfirm go ;;
    zypper) need_root && zypper --non-interactive install go ;;
    brew)   brew install go ;;
    *)      return 1 ;;
  esac
}

install_fuse() {
  local pm
  pm="$(detect_pm)"
  case "$pm" in
    apt)    need_root && apt-get install -y -qq fuse3 ;;
    dnf)    need_root && dnf install -y fuse3 ;;
    yum)    need_root && yum install -y fuse3 ;;
    pacman) need_root && pacman -S --noconfirm fuse3 ;;
    zypper) need_root && zypper --non-interactive install fuse3 ;;
    brew)   brew install macfuse || brew install fuse3 ;;
    *)      return 1 ;;
  esac
}

build_binary() {
  if (cd "$PROJECT_ROOT" && go build -o "$BIN_PATH" ./cmd/peerfold); then
    local size
    size="$(wc -c < "$BIN_PATH" | tr -d ' ')"
    ok "Build succeeded — ${BIN_PATH} ($((size / 1024 / 1024)) MB)"
    return 0
  fi
  return 1
}

add_to_path() {
  local marker="# peerfold"
  local line="export PATH=\"\$PATH:$PROJECT_ROOT\""
  local rc="${HOME_DIR}/.bashrc"
  [ -n "${ZSH_VERSION:-}" ] && rc="${HOME_DIR}/.zshrc"

  case ":$PATH:" in
    *":$PROJECT_ROOT:"*)
      ok "Already in PATH: $PROJECT_ROOT"
      return 0
      ;;
  esac

  if [ -f "$rc" ] && grep -qF "$marker" "$rc" 2>/dev/null; then
    ok "Already in PATH profile: $rc"
    return 0
  fi

  {
    echo ""
    echo "$marker"
    echo "$line"
  } >> "$rc"
  ok "Added to PATH in $rc"
  info "For the current terminal: export PATH=\"\$PATH:$PROJECT_ROOT\""
}

# ── Start ────────────────────────────────────────────────────

header

# ── [1/5] Go ─────────────────────────────────────────────────

section 1 "Go compiler"

if command -v go >/dev/null 2>&1; then
  ok "$(go version | sed 's/go version //') already installed"
else
  info "Go not found. Installing..."
  if install_go; then
    ok "Go installed successfully"
    if ! command -v go >/dev/null 2>&1; then
      warn "Close and reopen this terminal, then run the script again."
      exit 0
    fi
  else
    fail "Installation failed. Install manually:"
    fail "  Debian/Ubuntu:  sudo apt install golang-go"
    fail "  Fedora:         sudo dnf install golang"
    fail "  Arch:           sudo pacman -S go"
    fail "  macOS:          brew install go"
    fail "  Or download:    https://go.dev/dl/"
    exit 1
  fi
fi

# ── [2/5] FUSE (mount role) ──────────────────────────────────

section 2 "FUSE (needed to mount)"

if [ -e /dev/fuse ] || [ -e /dev/osxfuse ] || [ -e /dev/macfuse ]; then
  ok "FUSE device present"
elif command -v fusermount3 >/dev/null 2>&1 || command -v fusermount >/dev/null 2>&1; then
  ok "fusermount available"
else
  info "FUSE not found. Installing (used by 'peerfold mount')..."
  if install_fuse; then
    ok "FUSE installed successfully"
  else
    warn "Could not install FUSE automatically."
    warn "  Debian/Ubuntu:  sudo apt install fuse3"
    warn "  Fedora:         sudo dnf install fuse3"
    warn "  Arch:           sudo pacman -S fuse3"
    warn "Server and client work without FUSE; only mount needs it."
  fi
fi

# ── [3/5] Build ──────────────────────────────────────────────

section 3 "Build peerfold"

if [ -x "$BIN_PATH" ] && [ -z "$FORCE_REBUILD" ]; then
  info "Binary exists (built $(date -r "$BIN_PATH" '+%Y-%m-%d %H:%M' 2>/dev/null || echo 'earlier'))"
  printf "       Rebuild? [y/N] "
  read -r rebuild
  case "$rebuild" in
    y|Y)
      info "Compiling..."
      if build_binary; then :; else fail "Build failed"; exit 1; fi
      ;;
    *)
      ok "Keeping existing binary"
      ;;
  esac
else
  info "Compiling source code (first build downloads modules)..."
  if build_binary; then :; else fail "Build failed"; exit 1; fi
fi

# ── [4/5] PATH ───────────────────────────────────────────────

section 4 "Add to PATH"

add_to_path

# ── [5/5] Configuration ──────────────────────────────────────

section 5 "Configuration"

if [ -z "$NAME" ]; then
  NAME="$(hostname 2>/dev/null || echo peerfold)"
  info "Using hostname as client name: $NAME"
fi

if [ -z "$SHARE_DIR" ]; then
  SHARE_DIR="${HOME_DIR}/peerfold-demo/${NAME}"
  info "Using default share directory: $SHARE_DIR"
fi

CLIENT_DIR="${HOME_DIR}/peerfold-client-${NAME}"
if [ -d "$CLIENT_DIR" ]; then
  ok "Config directory exists: $CLIENT_DIR"
else
  mkdir -p "$CLIENT_DIR"
  ok "Created: $CLIENT_DIR"
fi

if [ -d "$SHARE_DIR" ]; then
  ok "Share directory exists: $SHARE_DIR"
else
  mkdir -p "$SHARE_DIR"
  ok "Created share directory: $SHARE_DIR"
fi

# ── Summary ──────────────────────────────────────────────────

echo ""
echo "  ------------------------------------"
echo "  Setup complete!"
echo ""
echo "  Start client:"
echo "    peerfold client"
echo ""
echo "  Or with flags:"
echo "    peerfold client --server 127.0.0.1:7000 --name ${NAME} --share docs=${SHARE_DIR}"
echo ""
echo "  Config: ${CLIENT_DIR}/config.json"
echo "  Log:    ${CLIENT_DIR}/log.txt"
echo ""
