#!/usr/bin/env bash
# setup-server.sh — peerfold server one-click setup (Linux / macOS)
# Installs Go + FUSE when possible, builds the binary, prepares config dirs.
# Mirrors setup-server.ps1.

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_PATH="${PROJECT_ROOT}/peerfold"
HOME_DIR="${HOME:-$(eval echo ~)}"
TOTAL=5

# ── Helpers ──────────────────────────────────────────────────

header() {
  echo ""
  echo "  peerfold"
  echo "  Federated filesystem — server setup"
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

# Package installs need root. When the script is not run as root, sudo does
# that instead of making the user re-run the whole script by hand.
SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
fi

need_root() {
  if [ "$(id -u)" -eq 0 ] || [ -n "$SUDO" ]; then
    return 0
  fi
  echo "       [!] Root needed to install packages. Install manually or run with sudo."
  return 1
}

# go_version_code prints major*1000+minor for the given go binary, or fails.
# It is what lets the script tell a Go 1.26 from a Go 1.27: `command -v go`
# only answers "is there a Go", while go.mod requires 1.27.
go_version_code() {
  local v major minor
  v="$("$1" env GOVERSION 2>/dev/null)" || return 1
  v="${v#go}"
  major="${v%%.*}"
  minor="${v#*.}"
  minor="${minor%%.*}"
  case "$major" in ''|*[!0-9]*) return 1 ;; esac
  case "$minor" in ''|*[!0-9]*) return 1 ;; esac
  echo $((10#$major * 1000 + 10#$minor))
}

# select_go picks the newest Go on the machine and puts it first on PATH.
# /usr/local/go is checked explicitly because a distro's /usr/bin/go can be
# older than an official toolchain that is already installed.
select_go() {
  local cand code best="" best_code=0
  for cand in "$(command -v go 2>/dev/null)" /usr/local/go/bin/go /usr/lib/golang/bin/go; do
    [ -n "$cand" ] && [ -x "$cand" ] || continue
    code="$(go_version_code "$cand")" || continue
    if [ "$code" -gt "$best_code" ]; then
      best="$cand"
      best_code="$code"
    fi
  done
  [ -n "$best" ] || return 1
  GO_BIN="$best"
  PATH="$(dirname "$best"):$PATH"
  export PATH
  return 0
}

install_go() {
  local pm
  pm="$(detect_pm)"
  case "$pm" in
    apt)    need_root && $SUDO apt-get update -qq && $SUDO apt-get install -y -qq golang-go ;;
    dnf)    need_root && $SUDO dnf install -y golang ;;
    yum)    need_root && $SUDO yum install -y golang ;;
    pacman) need_root && $SUDO pacman -S --noconfirm go ;;
    zypper) need_root && $SUDO zypper --non-interactive install go ;;
    brew)   brew install go ;;
    *)      return 1 ;;
  esac
}

install_fuse() {
  # Installs the runtime (fusermount3 + libfuse3), the headers the cgo build
  # needs, and gcc, which that build also needs: `-tags=fuse3` compiles C
  # against /usr/include/fuse3, and headers without a compiler do not build.
  local pm
  pm="$(detect_pm)"
  case "$pm" in
    apt)    need_root && $SUDO apt-get install -y -qq fuse3 libfuse3-dev gcc ;;
    dnf)    need_root && $SUDO dnf install -y fuse3 fuse3-devel gcc ;;
    yum)    need_root && $SUDO yum install -y fuse3 fuse3-devel gcc ;;
    pacman) need_root && $SUDO pacman -S --noconfirm fuse3 gcc ;;
    zypper) need_root && $SUDO zypper --non-interactive install fuse3 fuse3-devel gcc ;;
    brew)   brew install macfuse || brew install fuse3 ;;
    *)      return 1 ;;
  esac
}

# build_with runs one build, optionally through a different module proxy.
build_with() {
  local tags="$1" proxy="${2:-}"
  if [ -n "$proxy" ]; then
    (cd "$PROJECT_ROOT" && GOPROXY="$proxy" GOTOOLCHAIN=auto go build $tags -o "$BIN_PATH" ./cmd/peerfold)
  else
    (cd "$PROJECT_ROOT" && go build $tags -o "$BIN_PATH" ./cmd/peerfold)
  fi
}

build_binary() {
  # cgofuse on Linux defaults to libfuse2. The scripts install fuse3, so the
  # build asks for the fuse3 variant (CGO_CFLAGS picks /usr/include/fuse3).
  local tags=""
  case "$(uname -s | tr '[:upper:]' '[:lower:]')" in
    linux) tags="-tags=fuse3" ;;
  esac

  local built=0
  if build_with "$tags"; then
    built=1
  elif [ -z "${GOPROXY:-}" ]; then
    # proxy.golang.org is not reachable everywhere. A second attempt through a
    # mirror costs one failed build and turns a network wall into a working
    # install; an explicit GOPROXY from the user is respected, not replaced.
    info "Build failed; retrying with GOPROXY=https://goproxy.cn,direct ..."
    if build_with "$tags" "https://goproxy.cn,direct"; then
      built=1
    fi
  fi
  if [ "$built" -eq 1 ]; then
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
  # $SHELL is the login shell, which is what should be updated. ZSH_VERSION
  # would only be set when this script itself runs under an interactive zsh;
  # run as `bash setup-server.sh` it is always empty, and zsh users ended up
  # with the entry in .bashrc instead.
  local rc="${HOME_DIR}/.bashrc"
  case "$(basename "${SHELL:-bash}")" in
    zsh) rc="${HOME_DIR}/.zshrc" ;;
  esac

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

if ! select_go; then
  info "Go not found. Installing..."
  if install_go; then
    ok "Go installed successfully"
    if ! select_go; then
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

GO_CODE="$(go_version_code "$GO_BIN")"
ok "$("$GO_BIN" version | sed 's/go version //') at $GO_BIN"

if [ "$GO_CODE" -lt 1027 ]; then
  if [ "$GO_CODE" -lt 1021 ]; then
    fail "peerfold needs Go 1.27+, and this Go is too old to fetch one."
    fail "Install a current Go from https://go.dev/dl/ and run this again."
    exit 1
  fi
  # Go 1.21+ can fetch the toolchain go.mod asks for. The download goes through
  # GOPROXY like any module; build_binary retries through a mirror when the
  # default proxy is unreachable.
  ok "Go 1.27+ required; the matching toolchain will be fetched on first build"
  export GOTOOLCHAIN=auto
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
    warn "  Debian/Ubuntu:  sudo apt install fuse3 libfuse3-dev"
    warn "  Fedora:         sudo dnf install fuse3 fuse3-devel"
    warn "  Arch:           sudo pacman -S fuse3"
    warn "Server and client work without FUSE; only mount needs it."
  fi
fi

# The cgo build below (-tags=fuse3) compiles against the fuse3 headers. The
# device/fusermount checks above pass without them, so check separately.
if [ "$(uname -s)" = "Linux" ] && [ ! -f /usr/include/fuse3/fuse.h ]; then
  info "libfuse3 headers not found. Installing (needed to compile)..."
  if install_fuse; then
    ok "FUSE headers installed"
  else
    warn "Could not install fuse3 headers; the build below will fail."
    warn "  Debian/Ubuntu:  sudo apt install libfuse3-dev"
    warn "  RHEL/Fedora:    sudo dnf install fuse3-devel"
    warn "  Arch:           sudo pacman -S fuse3"
  fi
fi

# The cgo build also needs a C compiler, and fuse3-devel does not pull one in
# on every distribution. Checking here rather than letting the build fail is
# what turns "gcc: not found" into an installed package.
if [ "$(uname -s)" = "Linux" ] && ! command -v gcc >/dev/null 2>&1 && ! command -v cc >/dev/null 2>&1; then
  info "C compiler not found. Installing (needed by the cgo build)..."
  if install_fuse; then
    ok "C compiler installed"
  else
    warn "Could not install a C compiler; the build below may fail."
    warn "  Debian/Ubuntu:  sudo apt install gcc"
    warn "  RHEL/Fedora:    sudo dnf install gcc"
  fi
fi

# ── [3/5] Build ──────────────────────────────────────────────

section 3 "Build peerfold"

if [ -x "$BIN_PATH" ] && [ "${1:-}" != "--rebuild" ]; then
  info "Binary exists (built $(date -r "$BIN_PATH" '+%Y-%m-%d %H:%M' 2>/dev/null || echo 'earlier'))"
  rebuild=""
  if [ -t 0 ]; then
    printf "       Rebuild? [y/N] "
    read -r rebuild
  else
    info "Keeping existing binary (not a terminal; pass --rebuild to force)"
  fi
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

# ── [5/5] Config directories ─────────────────────────────────

section 5 "Configuration directories"

SERVER_DIR="${HOME_DIR}/peerfold-server"
if [ -d "$SERVER_DIR" ]; then
  ok "Already exists: $SERVER_DIR"
else
  mkdir -p "$SERVER_DIR"
  ok "Created: $SERVER_DIR"
fi

# ls/cat/mount read the server address from this file, so a directory alone
# leaves them saying "no server configured" after an otherwise successful
# setup. "listen" is what the server binds; "server" is what local clients
# dial. A config that already exists is never rewritten.
CONFIG_FILE="${SERVER_DIR}/config.json"
if [ -f "$CONFIG_FILE" ]; then
  ok "Already exists: $CONFIG_FILE"
  if ! grep -q '"server"' "$CONFIG_FILE" 2>/dev/null; then
    warn "No \"server\" field in the config; peerfold ls/cat/mount need one."
    info "Add e.g. \"server\": \"127.0.0.1:7000\""
  fi
else
  cat > "$CONFIG_FILE" <<'EOF'
{
  "listen": "127.0.0.1:7000",
  "server": "127.0.0.1:7000"
}
EOF
  ok "Created: $CONFIG_FILE"
fi

# ── Summary ──────────────────────────────────────────────────

echo ""
echo "  ------------------------------------"
echo "  Setup complete!"
echo ""
echo "  Start server:"
echo "    peerfold server"
echo ""
echo "  Config:  ${SERVER_DIR}/config.json"
echo "  Log:     ${SERVER_DIR}/log.txt"
echo ""
echo "  Next: run ./setup-client.sh on a client machine"
echo ""
