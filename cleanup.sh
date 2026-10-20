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
echo "    [x] Running processes (foreground & background)"
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

# Sweep by process name rather than PID files: foreground runs started with
# explicit flags (`peerfold server --listen ...`) never write a PID file, and a
# stale PID file could point at an unrelated process after the PID is reused.
# SIGTERM is the signal a mount handles by unmounting and flushing, so the
# mount role still shuts down cleanly.
list_pids() {
  pgrep -x peerfold 2>/dev/null || true
}

if command -v pgrep >/dev/null 2>&1; then
  if [ -n "$(list_pids)" ]; then
    kill -TERM $(list_pids) 2>/dev/null || true
    # A mount needs a moment to flush and unmount on SIGTERM.
    for _ in $(seq 1 50); do
      sleep 0.1
      [ -z "$(list_pids)" ] && break
    done
    # Anything that ignored SIGTERM (or is stuck in the kernel) goes now.
    if [ -n "$(list_pids)" ]; then
      kill -KILL $(list_pids) 2>/dev/null || true
      sleep 0.2
    fi
    if [ -n "$(list_pids)" ]; then
      info "Some peerfold processes are still running (owned by another user? try sudo)"
    else
      ok "Stopped peerfold processes"
    fi
  else
    ok "No running processes"
  fi
elif command -v pkill >/dev/null 2>&1; then
  pkill -x peerfold 2>/dev/null && sleep 1 && ok "Stopped peerfold processes" \
    || ok "No running processes"
else
  ok "pgrep/pkill not available; skip process stop"
fi

# ── [2/5] Config & logs ──────────────────────────────────────

section 2 "Remove config and log directories"

for dir in "${HOME_DIR}/peerfold-server" "${HOME_DIR}/peerfold-mount" "${HOME_DIR}/peerfold-client"; do
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
