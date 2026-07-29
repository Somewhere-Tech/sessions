#!/usr/bin/env bash
# Build the Tauri app and drop it into /Applications, replacing whatever
# is already there. Designed to be the one-command "publish my latest
# changes to my installed Sessions" loop while there's no auto-updater.
#
# DEFAULT BEHAVIOR: does NOT touch the running app. The bundle on disk
# gets replaced; macOS keeps the running process alive against the OLD
# code in memory until the user quits it themselves. This means a
# `ship` while you're typing into Claude will not interrupt anything;
# you pick up the new code the next time you cmd-Q + relaunch.
#
# Pass --restart if you actually want the build to quit the running app
# and relaunch the new one. Use that only when you know you're not in
# the middle of something.
#
# Skips the .dmg packaging step (--bundles app) — that's only useful for
# distribution and currently fails locally on bundle_dmg.sh anyway.

set -euo pipefail

RESTART=0
for arg in "$@"; do
  case "$arg" in
    --restart) RESTART=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 64 ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_NAME="Sessions"
PROCESS_NAME="sessions-app"
DST="/Applications/${APP_NAME}.app"
SRC="${ROOT}/src-tauri/target/release/bundle/macos/${APP_NAME}.app"

# Make rustup/cargo discoverable for callers (npm scripts, fresh shells)
# that don't source ~/.cargo/env on their own.
if [[ -z "${CARGO_HOME:-}" && -f "$HOME/.cargo/env" ]]; then
  # shellcheck disable=SC1091
  source "$HOME/.cargo/env"
fi

cd "$ROOT"

# Pre-flight: rust toolchain present?
if ! command -v cargo >/dev/null 2>&1; then
  echo "✗ cargo not in PATH. Install Rust via:" >&2
  echo "    curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y" >&2
  exit 1
fi

# Detect whether the app is currently running (LaunchServices view, not
# pgrep — avoids matching the build process or shell history).
is_running() {
  pgrep -x "${PROCESS_NAME}" >/dev/null 2>&1
}

if is_running && [[ "$RESTART" -eq 1 ]]; then
  echo "→ --restart given, quitting ${APP_NAME}…"
  osascript -e "tell application \"${APP_NAME}\" to quit" 2>/dev/null || true
  # Give the OS a beat to release file handles inside the .app bundle.
  sleep 0.3
elif is_running; then
  echo "→ ${APP_NAME} is running; bundle on disk will be replaced but the"
  echo "  process keeps the OLD code in memory. cmd-Q + relaunch when ready."
fi

# Build only the .app — skip dmg (saves 30s and avoids bundle_dmg.sh).
echo "→ building ${APP_NAME}.app (release)…"
npx tauri build --bundles app

if [[ ! -d "$SRC" ]]; then
  echo "✗ build finished but ${SRC} is missing. Aborting." >&2
  exit 2
fi

# Atomic-ish swap into /Applications. We rename the old bundle out of
# the way before deleting it, so a Spotlight reindex or a still-running
# instance's file handles can't half-delete the previous install. The
# running app keeps reading from its already-mapped pages even after
# its on-disk bundle moves to /tmp — macOS handles this fine.
if [[ -d "$DST" ]]; then
  STAMP="$(date +%s)"
  STASH="/tmp/${APP_NAME}.${STAMP}.app"
  echo "→ moving previous install aside (${STASH})…"
  mv "$DST" "$STASH"
  # Defer cleanup if the app is running so the OS still has the old
  # bundle on disk for any late-binding reads (icons, plist).
  if is_running; then
    (sleep 30 && rm -rf "$STASH") &
  else
    rm -rf "$STASH" &
  fi
fi

echo "→ copying ${SRC##*/} to /Applications…"
cp -R "$SRC" "$DST"

echo
echo "✓ shipped ${DST}"
SIZE_HUMAN="$(du -sh "$DST" | awk '{print $1}')"
echo "  size: ${SIZE_HUMAN}"

if is_running && [[ "$RESTART" -eq 1 ]]; then
  echo "→ relaunching ${APP_NAME}…"
  open "$DST"
elif is_running; then
  echo
  echo "  ${APP_NAME} is still running on the OLD code. To pick up this"
  echo "  build, cmd-Q the app and relaunch. Or run:"
  echo "    npm run ship -- --restart"
else
  echo "  open with:  open '${DST}'"
fi
