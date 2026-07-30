#!/bin/sh
set -eu

forbidden='
STATE.md
docs/WHY.md
docs/lanes/
docs/archive/
docs/CLOUD_VM.md
docs/CUTOVER.md
docs/CUTOVER_AUDIT_
docs/RUNBOOKS.md
ASSESSMENT.md
AUDIT.md
BACKFILL_NOTES.md
BACKFILL_SPEC.md
CODEXFIX_NOTES.md
CODEXFIX_SPEC.md
CODEX_CONTROLS.md
COMMERCIAL_GRADE_REVIEW.md
CONNECT_SPEC.md
CONTROLS_NOTES.md
CONTROLS_SPEC.md
DEPLOY_NOTES.md
DEPLOY_SPEC.md
HOOK_NOTES.md
HOOK_SPEC.md
NOTES.md
PKG_NOTES.md
PKG_SPEC.md
REMOTE_NOTES.md
REMOTE_SPEC.md
SCAN_NOTES.md
SCAN_SPEC.md
SPEC.md
frontend/CONNECT_NOTES.md
private/
internal-product/
services/somewhere/
services/cloud/
services/relay/
services/billing/
workers/hosted/
fly/
'

tracked="$(git ls-files)"
bad=
stale=
unexpected=
external_design_refs=

allowed_roots="$(cat scripts/public-paths.txt)"
while IFS= read -r tracked_path; do
  [ -n "$tracked_path" ] || continue
  root=${tracked_path%%/*}
  if [ "$root" = "$tracked_path" ]; then
    root=$tracked_path
  fi
  if ! printf '%s\n' "$allowed_roots" | grep -Fqx "$root"; then
    unexpected="${unexpected}${tracked_path}
"
  fi
done <<EOF
$tracked
EOF

while IFS= read -r path; do
  [ -n "$path" ] || continue
  matches="$(printf '%s\n' "$tracked" | awk -v prefix="$path" '
    index($0, prefix) == 1 { print }
  ')"
  if [ -n "$matches" ]; then
    bad="${bad}${matches}
"
  fi
done <<EOF
$forbidden
EOF

reference_forbidden='
STATE.md
docs/WHY.md
docs/lanes/
docs/archive/
docs/CLOUD_VM.md
docs/CUTOVER.md
docs/CUTOVER_AUDIT_
docs/RUNBOOKS.md
frontend/CONNECT_NOTES.md
'

while IFS= read -r path; do
  [ -n "$path" ] || continue
  refs="$(git grep -n -F "$path" -- \
    ':!.gitignore' \
    ':!scripts/check-public-tree.sh' 2>/dev/null || true)"
  if [ -n "$refs" ]; then
    stale="${stale}${refs}
"
  fi
done <<EOF
$reference_forbidden
EOF

external_design_refs="$(git grep -n -i -E \
  '(^|[^[:alnum:]_])(t3([ -]?code)?|happier|sessions-tmux|spotify)([^[:alnum:]_]|$)' -- \
  ':!.gitignore' \
  ':!scripts/check-public-tree.sh' 2>/dev/null || true)"

if [ -n "$bad" ]; then
  printf '%s\n' "private product artifacts are tracked in the public tree:" >&2
  printf '%s' "$bad" | sort -u >&2
  exit 1
fi

if [ -n "$unexpected" ]; then
  printf '%s\n' "tracked paths fall outside scripts/public-paths.txt:" >&2
  printf '%s' "$unexpected" | sort -u >&2
  exit 1
fi

if [ -n "$stale" ]; then
  printf '%s\n' "public files still refer to private product artifacts:" >&2
  printf '%s' "$stale" | sort -u >&2
  exit 1
fi

if [ -n "$external_design_refs" ]; then
  printf '%s\n' "public files still refer to external product-design sources:" >&2
  printf '%s\n' "$external_design_refs" >&2
  exit 1
fi

printf '%s\n' "public-tree path check passed"
