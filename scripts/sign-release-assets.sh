#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if (($# != 7)); then
  echo "usage: sign-release-assets.sh INPUT_DIR APP_STAGE CLI_STAGE TOOL_DIR NEW_OUTPUT_DIR VERSION NOTES_FILE" >&2
  exit 2
fi
INPUT="$1"
APP="$2/Sessions.app"
CLI="$3"
TOOL="$4"
OUTPUT="$5"
VERSION="$6"
NOTES="$7"
IDENTITY="${SESSIONS_SIGN_IDENTITY:?explicit Developer ID signing identity required}"
UPDATER_KEY="${SESSIONS_UPDATER_KEY_PATH:?explicit updater key path required}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && -f "$NOTES" && ! -e "$OUTPUT" ]]
[[ "$(uname -s)/$(uname -m)" == Darwin/arm64 ]]
[[ -d "$APP/Contents/Resources/runtime" && -d "$CLI" ]]
node "$ROOT/scripts/runtime-signing-manifest.mjs" verify "$APP/Contents/Resources/runtime" "$VERSION"
bundle_id="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$APP/Contents/Info.plist")"
app_version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$APP/Contents/Info.plist")"
[[ "$bundle_id" == tech.somewhere.sessions && "$app_version" == "$VERSION" ]]
mkdir -m 0700 "$OUTPUT"
mkdir -m 0700 "$OUTPUT/cli"

sign_binary() {
  local file="$1" identifier="$2"
  [[ -f "$file" && ! -L "$file" ]]
  codesign --force --timestamp --options runtime --identifier "$identifier" --sign "$IDENTITY" "$file"
  codesign --verify --strict --verbose=2 "$file"
}
for name in sessions sessionsd sessions-runner; do
  sign_binary "$APP/Contents/Resources/runtime/$name" "tech.somewhere.sessions.runtime.$name"
done
node "$ROOT/scripts/runtime-signing-manifest.mjs" render "$APP/Contents/Resources/runtime" "$VERSION"
node "$ROOT/scripts/runtime-signing-manifest.mjs" verify "$APP/Contents/Resources/runtime" "$VERSION"
codesign --force --timestamp --options runtime --sign "$IDENTITY" "$APP"
codesign --verify --deep --strict --verbose=2 "$APP"
for name in sessions sessionsd sessions-runner sessions-relay; do
  sign_binary "$CLI/$name" "tech.somewhere.sessions.$name"
done

notary_arguments=()
if [[ -n "${APPLE_API_KEY_PATH:-}" && -n "${APPLE_API_ISSUER:-}" && -n "${APPLE_API_KEY:-}" ]]; then
  notary_arguments=(--key "$APPLE_API_KEY_PATH" --key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER")
elif [[ -n "${APPLE_ID:-}" && -n "${APPLE_PASSWORD:-}" && -n "${APPLE_TEAM_ID:-}" ]]; then
  notary_arguments=(--apple-id "$APPLE_ID" --password "$APPLE_PASSWORD" --team-id "$APPLE_TEAM_ID")
else
  echo 'error: scoped notarization credentials are missing' >&2
  exit 1
fi
notarize() {
  local archive="$1" result="$2"
  xcrun notarytool submit "$archive" "${notary_arguments[@]}" --wait --timeout 30m --output-format json > "$result"
  node -e 'const fs=require("node:fs"); if(JSON.parse(fs.readFileSync(process.argv[1])).status!=="Accepted") process.exit(1)' "$result"
}
ditto -c -k --keepParent "$APP" "$OUTPUT/notarize-app.zip"
notarize "$OUTPUT/notarize-app.zip" "$OUTPUT/notarize-app.json"
ditto -c -k "$CLI" "$OUTPUT/notarize-cli.zip"
notarize "$OUTPUT/notarize-cli.zip" "$OUTPUT/notarize-cli.json"
xcrun stapler staple "$APP"
xcrun stapler validate "$APP"
spctl --assess --type execute --verbose=4 "$APP"
for name in sessions sessionsd sessions-runner sessions-relay; do
  # Apple's executable assessment is for app bundles, not bare CLI tools.
  # Require the standalone signed code's notarization ticket explicitly.
  # https://developer.apple.com/forums/thread/130560 (Other code)
  codesign --verify --strict --verbose=4 -R=notarized --check-notarization "$CLI/$name"
done
node "$ROOT/scripts/runtime-signing-manifest.mjs" verify "$APP/Contents/Resources/runtime" "$VERSION"
rm "$OUTPUT/notarize-app.zip" "$OUTPUT/notarize-app.json" "$OUTPUT/notarize-cli.zip" "$OUTPUT/notarize-cli.json"

COPYFILE_DISABLE=1 tar -czf "$OUTPUT/Sessions.app.tar.gz" -C "$(dirname "$APP")" Sessions.app
mkdir -m 0700 "$OUTPUT/verified-package"
tar -xzf "$OUTPUT/Sessions.app.tar.gz" -C "$OUTPUT/verified-package"
codesign --verify --deep --strict --verbose=2 "$OUTPUT/verified-package/Sessions.app"
xcrun stapler validate "$OUTPUT/verified-package/Sessions.app"
spctl --assess --type execute --verbose=4 "$OUTPUT/verified-package/Sessions.app"
python3 -c 'import shutil,sys; shutil.rmtree(sys.argv[1])' "$OUTPUT/verified-package"
node "$ROOT/scripts/sign-updater-artifact.mjs" "$TOOL" "$UPDATER_KEY" "$OUTPUT/Sessions.app.tar.gz"
node "$ROOT/scripts/verify-updater-signature.mjs" --public-key "$ROOT/release/updater.pub" \
  --artifact "$OUTPUT/Sessions.app.tar.gz" --signature "$OUTPUT/Sessions.app.tar.gz.sig"
node "$ROOT/scripts/render-updater-manifest.mjs" --version "$VERSION" \
  --artifact "$OUTPUT/Sessions.app.tar.gz" \
  --url "https://github.com/somewhere-tech/sessions/releases/download/v$VERSION/Sessions.app.tar.gz" \
  --target darwin-aarch64 --notes-file "$NOTES" --output "$OUTPUT/latest.json"
ditto -c -k --norsrc --noextattr --noqtn --noacl --keepParent "$APP" "$OUTPUT/Sessions_${VERSION}_darwin_arm64.zip"
python3 "$ROOT/scripts/unpack-release-app.py" "$OUTPUT/Sessions_${VERSION}_darwin_arm64.zip" "$OUTPUT/verified-zip"
codesign --verify --deep --strict --verbose=2 "$OUTPUT/verified-zip/Sessions.app"
xcrun stapler validate "$OUTPUT/verified-zip/Sessions.app"
spctl --assess --type execute --verbose=4 "$OUTPUT/verified-zip/Sessions.app"
node "$ROOT/scripts/runtime-signing-manifest.mjs" verify "$OUTPUT/verified-zip/Sessions.app/Contents/Resources/runtime" "$VERSION"
python3 -c 'import shutil,sys; shutil.rmtree(sys.argv[1])' "$OUTPUT/verified-zip"
COPYFILE_DISABLE=1 tar -czf "$OUTPUT/cli/sessions_${VERSION}_darwin_arm64.tar.gz" -C "$CLI" .
for platform in linux_arm64 linux_amd64; do
  cp "$INPUT/cli/sessions_${VERSION}_${platform}.tar.gz" "$OUTPUT/cli/"
done
for artifact in "$OUTPUT/Sessions.app.tar.gz" "$OUTPUT/Sessions_${VERSION}_darwin_arm64.zip" "$OUTPUT"/cli/*.tar.gz; do
  shasum -a 256 "$artifact" | sed 's#  .*/#  #' > "$artifact.sha256"
  (cd "$(dirname "$artifact")" && shasum -a 256 -c "$(basename "$artifact").sha256")
done
(cd "$OUTPUT" && shasum -a 256 Sessions.app.tar.gz Sessions.app.tar.gz.sig "Sessions_${VERSION}_darwin_arm64.zip" latest.json cli/*.tar.gz | sed 's#  cli/#  #') > "$OUTPUT/checksums.txt"
echo 'Signed artifacts verified.'
