#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

sizes=()

# This report covers handwritten production and build code. Generated provider
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
      sizes+=("$lines $file")
      ;;
  esac
done < <(git ls-files --cached --others --exclude-standard -- runtime frontend/src frontend/scripts src-tauri/src scripts)

printf 'Largest handwritten production and build files (report only):\n'
printf '  %s\n' "${sizes[@]}" | sort -nr | sed -n '1,10p'
printf 'File length is informational; function length and import boundaries are enforced separately.\n'
