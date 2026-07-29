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
# shellcheck disable=SC2086
git -C "$repo_root" archive "$revision" -- $paths | tar -x -C "$destination"
printf '%s\n' "exported allowlisted public tree from $revision to $destination"
