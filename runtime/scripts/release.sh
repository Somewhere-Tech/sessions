#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: release.sh [--version VERSION] [--output-dir DIR] [--dry-run]

Build every supported static binary with `make binaries`, create one release
archive per OS/architecture, write .sha256 files, and print formula checksums.

Options:
  --version VERSION  Artifact version, with or without a leading v. Defaults
                     to the exact git tag, or the current git description.
  --output-dir DIR   Archive destination (default: <repo>/dist-release).
  --dry-run          Print the build/package plan without running commands or
                     creating files.
  -h, --help         Show this help.
EOF
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
go_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$go_root/.." && pwd)"

version=""
output_arg="dist-release"
dry_run=false

while (($# > 0)); do
  case "$1" in
    --version)
      if (($# < 2)) || [[ "$2" == --* ]]; then
        echo "release: --version requires a value" >&2
        exit 2
      fi
      version="$2"
      shift 2
      ;;
    --output-dir)
      if (($# < 2)) || [[ "$2" == --* ]]; then
        echo "release: --output-dir requires a value" >&2
        exit 2
      fi
      output_arg="$2"
      shift 2
      ;;
    --dry-run)
      dry_run=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "release: unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$version" ]]; then
  version="$(git -C "$repo_root" describe --tags --exact-match 2>/dev/null || git -C "$repo_root" describe --tags --always --dirty)"
fi
version="${version#v}"
if [[ ! "$version" =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]]; then
  echo "release: invalid version '$version'" >&2
  exit 2
fi

if [[ "$output_arg" == /* ]]; then
  output_dir="$output_arg"
else
  output_dir="$repo_root/$output_arg"
fi

targets=(darwin/arm64 linux/arm64 linux/amd64)
commands=(sessions sessionsd sessions-runner)

echo "release version: $version"
echo "output directory: $output_dir"

if [[ "$dry_run" == true ]]; then
  echo "DRY RUN: SESSIONS_BUILD_VERSION=v$version DIST_GO_DIR=<scratch>/binaries make -C $go_root binaries"
  for target in "${targets[@]}"; do
    goos="${target%/*}"
    goarch="${target#*/}"
    archive="sessions_${version}_${goos}_${goarch}.tar.gz"
    echo "DRY RUN: package ${commands[*]} LICENSE README.md -> $output_dir/$archive"
    echo "DRY RUN: write $output_dir/$archive.sha256"
  done
  echo "DRY RUN: no commands executed and no files created"
  exit 0
fi

for command_name in make tar shasum; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "release: required command not found: $command_name" >&2
    exit 1
  fi
done

scratch_dir="$(mktemp -d "${TMPDIR:-/tmp}/sessions-release.XXXXXX")"
cleanup() {
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

binary_dir="$scratch_dir/binaries"
stage_root="$scratch_dir/stage"
mkdir -p "$binary_dir" "$stage_root" "$output_dir"

SESSIONS_BUILD_VERSION="v$version" DIST_GO_DIR="$binary_dir" make -C "$go_root" binaries

for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  stage_dir="$stage_root/${goos}_${goarch}"
  archive_name="sessions_${version}_${goos}_${goarch}.tar.gz"
  archive_path="$output_dir/$archive_name"
  checksum_path="$archive_path.sha256"
  mkdir -p "$stage_dir"

  for command_name in "${commands[@]}"; do
    source_path="$binary_dir/${command_name}-${goos}-${goarch}"
    if [[ ! -x "$source_path" ]]; then
      echo "release: build did not produce executable $source_path" >&2
      exit 1
    fi
    cp "$source_path" "$stage_dir/$command_name"
    chmod 0755 "$stage_dir/$command_name"
  done
  cp "$repo_root/LICENSE" "$repo_root/README.md" "$stage_dir/"

  tar -czf "$archive_path" -C "$stage_dir" .
  digest="$(shasum -a 256 "$archive_path" | awk '{print $1}')"
  printf '%s  %s\n' "$digest" "$archive_name" >"$checksum_path"
  printf '%s  %s\n' "$digest" "$archive_name"
done

echo "release archives written to $output_dir"
