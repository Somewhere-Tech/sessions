#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
go_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$go_root/.." && pwd)"
scratch_dir="$(mktemp -d "${TMPDIR:-/tmp}/sessions-cli-docs.XXXXXX")"
trap 'rm -rf "$scratch_dir"' EXIT

sessions_bin="$scratch_dir/sessions"
output_tmp="$scratch_dir/CLI.md"
output="$repo_root/docs/CLI.md"

(cd "$go_root" && go build -buildvcs=false -o "$sessions_bin" ./cmd/sessions)
"$sessions_bin" docs >"$output_tmp"

mv "$output_tmp" "$output"
