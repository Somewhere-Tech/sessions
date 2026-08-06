#!/bin/sh
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  printf '%s\n' "usage: scripts/export-public-tree.sh DESTINATION [REVISION]" >&2
  exit 2
fi

destination=$1
revision=${2:-HEAD}
repo_root=$(git rev-parse --show-toplevel)
manifest="$repo_root/scripts/public-paths.txt"

if [ -e "$destination" ]; then
  printf '%s\n' "destination already exists: $destination" >&2
  exit 1
fi

"$repo_root/scripts/check-public-tree.sh"

paths=
while IFS= read -r path; do
  [ -n "$path" ] || continue
  paths="${paths}${path}
"
done < "$manifest"

mkdir -p "$destination"
# The manifest is the publication boundary. Archive only those paths from one
# committed revision, then let the caller create the new root commit.
#
# This is /bin/sh, so there is no `pipefail`: piping `git archive` straight into
# `tar -x` reports only tar's status. A failing `git archive` (bad revision, a
# manifest path that no longer exists) produced an empty stream, tar exited 0,
# and the script printed its success message having exported nothing. Write the
# archive to a temporary file first so git's own exit status is checked, then
# extract, then prove the extraction is non-empty before claiming success.
scratch_dir=$(mktemp -d "${TMPDIR:-/tmp}/sessions-public-tree.XXXXXX")
trap 'rm -rf "$scratch_dir"' EXIT INT TERM
archive_file="$scratch_dir/public-tree.tar"

# shellcheck disable=SC2086
if ! git -C "$repo_root" archive --format=tar -o "$archive_file" "$revision" -- $paths; then
  printf '%s\n' "git archive failed for revision $revision; nothing was exported" >&2
  exit 1
fi
if [ ! -s "$archive_file" ]; then
  printf '%s\n' "git archive produced an empty archive for revision $revision" >&2
  exit 1
fi
if ! tar -xf "$archive_file" -C "$destination"; then
  printf '%s\n' "extracting the public tree archive failed" >&2
  exit 1
fi

exported_count=$(find "$destination" -type f | wc -l | tr -d ' ')
if [ "$exported_count" -eq 0 ]; then
  printf '%s\n' "no files were exported to $destination; refusing to report success" >&2
  exit 1
fi

printf '%s\n' "exported allowlisted public tree from $revision to $destination ($exported_count files)"
