#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
go_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$go_root/.." && pwd)"
frontend_dir="$repo_root/frontend"
frontend_dist="$frontend_dir/dist"
frontend_bin="$frontend_dir/node_modules/.bin"
asset_dir="$go_root/internal/webassets/dist"
out_dir="${DIST_GO_DIR:-$go_root/dist-go}"
embed_ui=true

if (($# > 1)); then
  echo "usage: build-binaries.sh [--no-ui]" >&2
  exit 2
fi
if (($# == 1)); then
  if [[ "$1" != "--no-ui" ]]; then
    echo "build-binaries: unknown option: $1" >&2
    echo "usage: build-binaries.sh [--no-ui]" >&2
    exit 2
  fi
  embed_ui=false
fi

required_commands=(go git)
if [[ "$embed_ui" == true ]]; then
  required_commands+=(npm)
fi
for command_name in "${required_commands[@]}"; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "build-binaries: required command not found: $command_name" >&2
    exit 1
  fi
done

if [[ "$embed_ui" == true ]]; then
  for frontend_command in tsc vite; do
    if [[ ! -x "$frontend_bin/$frontend_command" ]]; then
      echo "build-binaries: missing $frontend_bin/$frontend_command; run npm --prefix frontend ci" >&2
      exit 1
    fi
  done
  echo "> building frontend via npm (frontend/node_modules/.bin)"
  PATH="$frontend_bin:$PATH" npm --prefix "$frontend_dir" run build
  if [[ ! -f "$frontend_dist/index.html" ]]; then
    echo "build-binaries: frontend build did not produce $frontend_dist/index.html" >&2
    exit 1
  fi

  mkdir -p "$asset_dir"
  find "$asset_dir" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
  cp -R "$frontend_dist"/. "$asset_dir"/
else
  echo "> skipping frontend build and UI embedding (--no-ui)"
fi

mkdir -p "$out_dir"

version="${SESSIONS_BUILD_VERSION:-$(git -C "$repo_root" describe --tags --always --dirty 2>/dev/null || printf 'dev')}"
if [[ ! "$version" =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]]; then
  echo "build-binaries: invalid SESSIONS_BUILD_VERSION: $version" >&2
  exit 2
fi
ldflags="-s -w -X main.version=$version"

build_binary() {
  local goos="$1"
  local goarch="$2"
  local command_name="$3"
  local tags="$4"
  local output="$out_dir/${command_name}-${goos}-${goarch}"
  local binary_ldflags="$ldflags -buildid=sessions/$version/$command_name/$goos/$goarch"
  echo "> building ${command_name}-${goos}-${goarch} (version $version)"
  if [[ -n "$tags" ]]; then
    (
      cd "$go_root"
      CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
        go build -trimpath -tags "$tags" -ldflags "$binary_ldflags" -o "$output" "./cmd/$command_name"
    )
  else
    (
      cd "$go_root"
      CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
        go build -trimpath -ldflags "$binary_ldflags" -o "$output" "./cmd/$command_name"
    )
  fi
}

for target in darwin/arm64 linux/arm64 linux/amd64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  build_binary "$goos" "$goarch" sessions ""
  daemon_tags=""
  if [[ "$embed_ui" == true ]]; then
    daemon_tags="embedui"
  fi
  build_binary "$goos" "$goarch" sessionsd "$daemon_tags"
  build_binary "$goos" "$goarch" sessions-runner ""
done

# Sign darwin binaries when an identity is configured (SESSIONS_SIGN_IDENTITY, a
# SHA-1 hash or exact name from `security find-identity -v -p codesigning`).
# A stable identifier per binary keeps the macOS TCC identity constant across
# rebuilds, so file-access grants are asked ONCE instead of on every build.
if [[ -z "${SESSIONS_SIGN_IDENTITY:-}" && -r "$HOME/.config/sessions/sign-identity" ]]; then
  SESSIONS_SIGN_IDENTITY="$(head -n1 "$HOME/.config/sessions/sign-identity")"
fi
if [[ -n "${SESSIONS_SIGN_IDENTITY:-}" ]]; then
  for command_name in sessions sessionsd sessions-runner; do
    signed="$out_dir/${command_name}-darwin-arm64"
    echo "> signing ${command_name}-darwin-arm64 (identity ${SESSIONS_SIGN_IDENTITY:0:8}…)"
    codesign --force --timestamp --options runtime \
      --identifier "tech.somewhere.sessions.$command_name" \
      --sign "$SESSIONS_SIGN_IDENTITY" "$signed"
    codesign --verify --strict "$signed"
  done
else
  echo "> darwin binaries UNSIGNED (set SESSIONS_SIGN_IDENTITY to sign; unsigned rebuilds re-trigger macOS file-access dialogs)"
fi

darwin_daemon="$out_dir/sessionsd-darwin-arm64"
darwin_runner="$out_dir/sessions-runner-darwin-arm64"
daemon_label="${SESSIONS_DAEMON_LABEL:-tech.somewhere.sessions.dev.daemon}"
daemon_host="${SESSIONS_HOST:-127.0.0.1}"
daemon_port="${SESSIONS_PORT:-8787}"
if [[ ! "$daemon_label" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*$ ]]; then
  echo "build-binaries: invalid SESSIONS_DAEMON_LABEL: $daemon_label" >&2
  exit 2
fi
plist="$out_dir/${daemon_label}-darwin-arm64.plist"
sed \
  -e "s|@DAEMON_LABEL@|$daemon_label|g" \
  -e "s|@SESSIONS_BINARY@|$darwin_daemon|g" \
  -e "s|@SESSIONS_RUNNER@|$darwin_runner|g" \
  -e "s|@SESSIONS_HOST@|$daemon_host|g" \
  -e "s|@SESSIONS_PORT@|$daemon_port|g" \
  -e "s|@WORKING_DIRECTORY@|$out_dir|g" \
  -e "s|@LOG_PATH@|$out_dir/${daemon_label}-darwin-arm64.log|g" \
  "$script_dir/launchd.plist.tmpl" >"$plist"
chmod 0644 "$plist"

echo "> wrote $plist"
echo "> binaries available in $out_dir"
