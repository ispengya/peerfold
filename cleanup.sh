#!/usr/bin/env bash
# cleanup.sh — Remove peerfold data, configs, and build artifacts
# Only removes peerfold's own files. Does NOT touch system dependencies
# (FUSE, PATH, source code).

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOME_DIR="${HOME:-$(eval echo ~)}"

# ── Helpers ──────────────────────────────────────────────────

header() {
  echo ""
  echo "  peerfold"
  echo "  Federated filesystem — cleanup"
  echo "  ------------------------------------"
  echo ""
}

section() {
  echo ""
  echo "  [$1/5] $2"
}

ok()      { echo "       [OK] $1"; }
info()    { echo "       $1"; }
gone()    { echo "       [OK] Removed: $1"; }
notfound(){ echo "       ... Not found: $1"; }

# ── Start ────────────────────────────────────────────────────

header

echo "  This will remove peerfold's own data:"
echo ""
echo "    [x] Server & client processes"
echo "    [x] Config directories and logs"
echo "    [x] Auto-start entries (XDG)"
echo "    [x] Demo / test files"
echo "    [x] Build artifacts (./peerfold)"
echo "    [x] Installed command (~/.local/bin/peerfold)"
echo ""
echo "  Not touched: FUSE, system PATH, source code"
echo ""

printf "  Continue? [y/N] "
read -r confirm
case "$confirm" in
  y|Y|yes|YES) ;;
  *) echo ""; echo "  Cancelled."; exit 0 ;;
esac

# ── [1/5] Processes ──────────────────────────────────────────

section 1 "Stop peerfold processes"

if command -v pkill >/dev/null 2>&1; then
  if pkill -f "${PROJECT_ROOT}/peerfold" 2>/dev/null || pkill -x peerfold 2>/dev/null; then
    sleep 1
    ok "Stopped peerfold processes"
  else
    ok "No running processes"
  fi
else
  ok "pkill not available; skip process stop"
fi

# ── [2/5] Config & logs ──────────────────────────────────────

section 2 "Remove config and log directories"

for dir in "${HOME_DIR}/peerfold-server"; do
  if [ -d "$dir" ]; then
    rm -rf "$dir"
    gone "$dir"
  else
    notfound "$dir"
  fi
done

for dir in "${HOME_DIR}"/peerfold-client-*; do
  if [ -d "$dir" ]; then
    rm -rf "$dir"
    gone "$dir"
  fi
done

for f in "${HOME_DIR}"/peerfold-*.json; do
  if [ -f "$f" ]; then
    rm -f "$f"
    gone "$f"
  fi
done

# ── [3/5] Auto-start entries ─────────────────────────────────

section 3 "Remove auto-start registrations"

AUTOSTART_DIR="${HOME_DIR}/.config/autostart"
removed=0
for f in "${AUTOSTART_DIR}"/peerfold-*.desktop; do
  if [ -f "$f" ]; then
    rm -f "$f"
    gone "$f"
    removed=1
  fi
done
if [ "$removed" -eq 0 ]; then
  ok "No auto-start entries found"
fi

# ── [4/5] Demo files ─────────────────────────────────────────

section 4 "Remove demo directories"

for demo in "${HOME_DIR}/peerfold-demo" "/tmp/peerfold-demo"; do
  if [ -d "$demo" ]; then
    rm -rf "$demo"
    gone "$demo"
  else
    notfound "$demo"
  fi
done

# ── [5/5] Build artifacts ────────────────────────────────────

section 5 "Remove build artifacts"

# The installed command is a symlink, so -f alone is not enough: a symlink
# whose target is already gone still has to be removed.
for bin in "${PROJECT_ROOT}/peerfold" "${PROJECT_ROOT}/peerfold.exe" "${HOME_DIR}/.local/bin/peerfold"; do
  if [ -e "$bin" ] || [ -L "$bin" ]; then
    rm -f "$bin"
    gone "$bin"
  else
    notfound "$bin"
  fi
done

# ── Summary ──────────────────────────────────────────────────

echo ""
echo "  ------------------------------------"
echo "  Cleanup complete!"
echo ""
echo "  Removed: processes, configs, logs, auto-start,"
echo "           demo files, build artifacts, installed command"
echo ""
echo "  Untouched: FUSE, system PATH, source code"
echo ""
