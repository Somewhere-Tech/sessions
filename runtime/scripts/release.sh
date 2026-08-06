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
  --allow-dirty      Build release archives from a dirty worktree. The
                     artifacts are then NOT reproducible from any commit;
                     never use this for a published release.
  -h, --help         Show this help.
EOF
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
go_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$go_root/.." && pwd)"

version=""
output_arg="dist-release"
dry_run=false
allow_dirty=false

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
    --allow-dirty)
      allow_dirty=true
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

# This script had no clean-tree gate at all: it would happily build publishable,
# checksummed tarballs out of uncommitted edits and label them with a release
# version, so "released" could not be traced back to any reviewed source state.
# Use the same strict definition as scripts/build-app-runtime.sh (tracked AND
# untracked), scoped to the inputs that actually determine archive contents —
# `make binaries` builds the Go commands from runtime/ and embeds frontend/, and
# LICENSE/README.md are copied verbatim into every archive. Scoping keeps the
# gate from tripping on its own output directory.
release_source_paths=(runtime frontend LICENSE README.md)
source_state="$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all -- "${release_source_paths[@]}")"
if [[ -n "$source_state" ]]; then
  if [[ "$allow_dirty" == true || "$dry_run" == true ]]; then
    # A dry run creates nothing, so it warns instead of failing; it still has to
    # report the dirt so the preview is not mistaken for a clean release plan.
    echo "release: WARNING: worktree is DIRTY." >&2
    echo "release: WARNING: archives built from it are not reproducible from any commit." >&2
    printf '%s\n' "$source_state" >&2
  else
    echo "release: refusing to build release archives from a dirty worktree" >&2
    printf '%s\n' "$source_state" >&2
    echo "release: commit or stash the above, or pass --allow-dirty for a local build" >&2
    exit 1
  fi
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

  # Build the archive in scratch, verify it there, and only then publish it.
  # Writing straight to the publish path and then checksumming whatever landed
  # means a truncated or partial archive gets a .sha256 of its own truncated
  # bytes and passes `shasum -c` forever after. The expected digest must be
  # established from a verified archive and then confirmed at the publish path.
  #
  # COPYFILE_DISABLE=1: Apple's bsdtar serialises extended attributes and
  # resource forks as ._ AppleDouble members. Those must never appear in a
  # Linux tarball (and would change the digest of an otherwise identical build).
  archive_tmp="$scratch_dir/$archive_name"
  COPYFILE_DISABLE=1 tar -czf "$archive_tmp" -C "$stage_dir" .

  # Reading the whole archive back proves the gzip stream is complete and its
  # CRC is intact — a truncated file fails here instead of being published.
  if ! archive_members="$(tar -tzf "$archive_tmp")"; then
    echo "release: archive $archive_name is unreadable or truncated" >&2
    exit 1
  fi
  actual_members="$(printf '%s\n' "$archive_members" \
    | sed -e 's|^\./||' -e 's|/$||' \
    | grep -v '^$' \
    | sort)"
  expected_members="$(printf '%s\n' "${commands[@]}" LICENSE README.md | sort)"
  if [[ "$actual_members" != "$expected_members" ]]; then
    echo "release: $archive_name does not contain exactly the expected files" >&2
    echo "expected:" >&2
    printf '%s\n' "$expected_members" | sed 's/^/  /' >&2
    echo "found:" >&2
    printf '%s\n' "$actual_members" | sed 's/^/  /' >&2
    exit 1
  fi

  # Establish the expected digest from the verified scratch archive, publish,
  # then confirm the published bytes match that expected value.
  expected_digest="$(shasum -a 256 "$archive_tmp" | awk '{print $1}')"
  mv "$archive_tmp" "$archive_path"
  published_digest="$(shasum -a 256 "$archive_path" | awk '{print $1}')"
  if [[ "$published_digest" != "$expected_digest" ]]; then
    echo "release: $archive_path does not match the verified archive" >&2
    echo "  expected $expected_digest" >&2
    echo "  found    $published_digest" >&2
    rm -f "$archive_path"
    exit 1
  fi

  printf '%s  %s\n' "$expected_digest" "$archive_name" >"$checksum_path"
  # Confirm the checksum file we just wrote actually validates the published
  # archive, so "checksum written" is never reported without being checked.
  if ! (cd "$output_dir" && shasum -a 256 -c "$archive_name.sha256" >/dev/null); then
    echo "release: checksum file $checksum_path does not verify $archive_path" >&2
    exit 1
  fi
  printf '%s  %s\n' "$expected_digest" "$archive_name"
done

echo "release archives written and checksum-verified in $output_dir"
