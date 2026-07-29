#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEY_PATH="${SESSIONS_UPDATER_KEY_PATH:-$HOME/.config/sessions/sessions-updater.key}"
NOTARIZATION_KEYCHAIN_SERVICE="${SESSIONS_NOTARIZATION_KEYCHAIN_SERVICE:-tech.somewhere.sessions.notarization}"
EXPECTED_PUBLIC_KEY="$ROOT/release/updater.pub"
VERSION=""
NOTES_FILE=""
DRY_RUN=0

usage() {
  cat >&2 <<'EOF'
Usage: scripts/release-app.sh --version X.Y.Z --notes-file PATH [--dry-run]

Builds the signed, notarized Apple Silicon Sessions.app updater artifact and
renders a Tauri latest.json manifest. It does not publish either artifact.

Required release secrets:
  SESSIONS_UPDATER_KEY_PATH (defaults to ~/.config/sessions/sessions-updater.key)
  APPLE_ID + APPLE_PASSWORD + APPLE_TEAM_ID, or App Store Connect API key vars

On macOS, APPLE_ID and APPLE_PASSWORD are loaded automatically when a generic
password exists in the login Keychain with service
"tech.somewhere.sessions.notarization". The Keychain account is the Apple ID.
EOF
}

while (($#)); do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --notes-file) NOTES_FILE="${2:-}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; echo "error: unknown argument $1" >&2; exit 2 ;;
  esac
done

if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
  usage
  echo "error: --version must be a semantic version without a leading v" >&2
  exit 2
fi
if [[ -z "$NOTES_FILE" || ! -f "$NOTES_FILE" ]]; then
  usage
  echo "error: --notes-file must name an existing file" >&2
  exit 2
fi

CONFIG_VERSION="$(node -p "require('$ROOT/src-tauri/tauri.conf.json').version")"
FRONTEND_VERSION="$(node -p "require('$ROOT/frontend/package.json').version")"
CARGO_VERSION="$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$ROOT/src-tauri/Cargo.toml" | head -1)"
for pair in "tauri.conf.json:$CONFIG_VERSION" "frontend/package.json:$FRONTEND_VERSION" "Cargo.toml:$CARGO_VERSION"; do
  if [[ "${pair#*:}" != "$VERSION" ]]; then
    echo "error: ${pair%%:*} is ${pair#*:}, expected $VERSION" >&2
    exit 1
  fi
done

if [[ "$(uname -s)" != "Darwin" || "$(uname -m)" != "arm64" ]]; then
  echo "error: the current release lane produces notarized darwin-aarch64 artifacts" >&2
  exit 1
fi
if [[ ! -f "$KEY_PATH" || ! -f "$KEY_PATH.pub" ]]; then
  echo "error: updater signing keypair is missing at $KEY_PATH{,.pub}" >&2
  exit 1
fi
if [[ "$(tr -d '\r\n' < "$KEY_PATH.pub")" != "$(tr -d '\r\n' < "$EXPECTED_PUBLIC_KEY")" ]]; then
  echo "error: updater public key does not match the key pinned in the app" >&2
  exit 1
fi

ARTIFACT_NAME="Sessions.app.tar.gz"
ARTIFACT_URL="https://github.com/somewhere-tech/sessions/releases/download/v${VERSION}/${ARTIFACT_NAME}"
OUTPUT_DIR="$ROOT/release/out/v${VERSION}"
APP="$ROOT/src-tauri/target/release/bundle/macos/Sessions.app"
ARTIFACT="$ROOT/src-tauri/target/release/bundle/macos/$ARTIFACT_NAME"

if ((DRY_RUN)); then
  printf 'release version: %s\n' "$VERSION"
  printf 'updater target: darwin-aarch64\n'
  printf 'immutable artifact: %s\n' "$ARTIFACT_URL"
  printf 'manifest output: %s/latest.json\n' "$OUTPUT_DIR"
  printf 'dry run: no build, notarization, publication, or app installation performed\n'
  exit 0
fi

if ! git -C "$ROOT" diff --quiet || ! git -C "$ROOT" diff --cached --quiet; then
  echo "error: release builds require a clean reviewed worktree" >&2
  exit 1
fi

# Prefer an explicitly supplied credential (local shell or CI). For repeat
# local releases, fall back to the login Keychain without ever printing or
# persisting the app-specific password in this checkout. `security` can locate
# the account attribute without reading the secret, then returns only the
# password to this process for the duration of the release command.
if [[ -z "${APPLE_PASSWORD:-}" ]] && command -v security >/dev/null 2>&1; then
  keychain_record="$(security find-generic-password -s "$NOTARIZATION_KEYCHAIN_SERVICE" 2>/dev/null || true)"
  keychain_account="$(printf '%s\n' "$keychain_record" | sed -n 's/^[[:space:]]*"acct"<blob>="\(.*\)"$/\1/p' | head -1)"
  if [[ -n "$keychain_account" ]]; then
    keychain_password="$(security find-generic-password -a "$keychain_account" -s "$NOTARIZATION_KEYCHAIN_SERVICE" -w 2>/dev/null || true)"
    if [[ -n "$keychain_password" ]]; then
      export APPLE_ID="${APPLE_ID:-$keychain_account}"
      export APPLE_PASSWORD="$keychain_password"
      export APPLE_TEAM_ID="${APPLE_TEAM_ID:-7GW9T5ZWW8}"
    fi
  fi
fi

has_apple_id=0
has_api_key=0
if [[ -n "${APPLE_ID:-}" && -n "${APPLE_PASSWORD:-}" && -n "${APPLE_TEAM_ID:-}" ]]; then
  has_apple_id=1
fi
if [[ -n "${APPLE_API_ISSUER:-}" && ( -n "${APPLE_API_KEY:-}" || -n "${APPLE_API_KEY_PATH:-}" ) ]]; then
  has_api_key=1
fi
if ((has_apple_id == 0 && has_api_key == 0)); then
  echo "error: notarization credentials are missing; create an Apple app-specific password or configure an App Store Connect API key" >&2
  exit 1
fi

export TAURI_SIGNING_PRIVATE_KEY="$KEY_PATH"
export TAURI_SIGNING_PRIVATE_KEY_PASSWORD="${TAURI_SIGNING_PRIVATE_KEY_PASSWORD:-}"
npm --prefix "$ROOT" exec tauri build -- --bundles app

codesign --verify --deep --strict --verbose=2 "$APP"
while IFS= read -r binary; do
  codesign --verify --strict --verbose=2 "$binary"
done < <(find "$APP/Contents/Resources/runtime" -type f -perm -111 -print | sort)
xcrun stapler validate "$APP"
spctl --assess --type execute --verbose=4 "$APP"

if [[ ! -f "$ARTIFACT" || ! -f "$ARTIFACT.sig" ]]; then
  echo "error: Tauri did not produce $ARTIFACT{,.sig}" >&2
  exit 1
fi

mkdir -p "$OUTPUT_DIR"
artifact_digest="$(shasum -a 256 "$ARTIFACT" | awk '{print $1}')"
printf '%s  %s\n' "$artifact_digest" "$ARTIFACT_NAME" > "$OUTPUT_DIR/$ARTIFACT_NAME.sha256"
node "$ROOT/scripts/render-updater-manifest.mjs" \
  --version "$VERSION" \
  --artifact "$ARTIFACT" \
  --url "$ARTIFACT_URL" \
  --target darwin-aarch64 \
  --notes-file "$NOTES_FILE" \
  --output "$OUTPUT_DIR/latest.json"

printf 'release artifacts verified; upload these immutable files first:\n'
printf '  %s\n  %s\n' "$ARTIFACT" "$ARTIFACT.sig"
printf 'then patch only releases/latest.json on sessions.somewhere.tech with:\n'
printf '  %s/latest.json\n' "$OUTPUT_DIR"
