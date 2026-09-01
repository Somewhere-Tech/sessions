#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

max_lines=1000
failures=()

# This is a guard for handwritten production and build code. Generated provider
# schemas, compatibility fixtures, public contracts, and tests are deliberately
# excluded: their size is controlled by their source or by scenario coverage,
# not by adding arbitrary seams to the shipped implementation.
while IFS= read -r file; do
  case "$file" in
    runtime/testdata/*|runtime/CONTRACT/*|runtime/internal/codexapp/protoschema/*)
      continue
      ;;
    *_test.go|*_test.rs|*.test.ts|*.test.tsx|*_tests.rs|*.generated.*|*_generated.go)
      continue
      ;;
  esac

  case "$file" in
    *.go|*.rs|*.ts|*.tsx|*.css|*.mjs|*.cjs|*.sh)
      lines="$(wc -l < "$file" | tr -d ' ')"
      if (( lines > max_lines )); then
        failures+=("$lines $file")
      fi
      ;;
  esac
done < <(git ls-files --cached --others --exclude-standard -- runtime frontend/src frontend/scripts src-tauri/src scripts)

if (( ${#failures[@]} > 0 )); then
  printf 'Handwritten source files must stay at or below %d lines:\n' "$max_lines" >&2
  printf '  %s\n' "${failures[@]}" | sort -nr >&2
  printf 'Split the responsibility into a named module; do not raise the cap.\n' >&2
  exit 1
fi

printf 'Source-size guard passed: handwritten production files are at most %d lines.\n' "$max_lines"
