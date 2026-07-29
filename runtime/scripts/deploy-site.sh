#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
go_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$go_root/.." && pwd)"
frontend_dir="$repo_root/frontend"
frontend_dist="$frontend_dir/dist"
project="${SOMEWHERE_PROJECT:-sessions}"

if (($# != 0)); then
  echo "usage: deploy-site.sh" >&2
  echo "override the scratch output with SITE_STAGE_DIR=/absolute/path" >&2
  exit 2
fi

for command_name in npm cp find mkdir mktemp; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "deploy-site: required command not found: $command_name" >&2
    exit 1
  fi
done

stage_dir="${SITE_STAGE_DIR:-}"
if [[ -z "$stage_dir" ]]; then
  stage_dir="$(mktemp -d "${TMPDIR:-/tmp}/sessions-site-stage.XXXXXX")"
else
  case "$stage_dir" in
    /|"$repo_root"|"$repo_root/site")
      echo "deploy-site: refusing unsafe stage directory: $stage_dir" >&2
      exit 2
      ;;
  esac
  mkdir -p "$stage_dir"
  if [[ -n "$(find "$stage_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
    echo "deploy-site: stage directory must be empty: $stage_dir" >&2
    exit 2
  fi
fi

echo "> building the complete static frontend"
npm --prefix "$frontend_dir" run build

if [[ ! -f "$frontend_dist/index.html" ]]; then
  echo "deploy-site: frontend build did not produce $frontend_dist/index.html" >&2
  exit 1
fi

cp -R "$frontend_dist"/. "$stage_dir"/

# sessions remote enable links historically land on /connect.html. Make that
# path boot the same app shell and preserve its #endpoint=...&token=...
# fragment, while / remains the canonical hosted entry.
cp "$stage_dir/index.html" "$stage_dir/connect.html"

echo "> staged hosted app at $stage_dir"
echo "> entrypoints: index.html and connect.html"
echo
echo "No deployment was run. Review the staged files, then deploy with:"
printf 'somewhere deploy --project %q --scope static --prebuilt %q\n' "$project" "$stage_dir"
echo
echo "For a remote change preview only, append --dry-run to that command."
