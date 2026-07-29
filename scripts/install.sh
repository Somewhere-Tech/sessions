#!/usr/bin/env bash
set -euo pipefail

cat >&2 <<'EOF'
scripts/install.sh was retired with the Node-daemon deploy path; no changes were made.

For local Sessions.app development, use:
  npm run bootstrap
  npm run tauri:build

For the current standalone Go development daemon, install adjacent sessions,
sessionsd, and sessions-runner binaries and run:
  sessions install

Public app release and update work follows docs/RELEASE.md and
docs/NATIVE_APP.md. The production mini must not be changed by this script.
EOF
exit 2
