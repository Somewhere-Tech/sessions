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
legacy_branding=

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

# git grep exits 1 for "no match" and 128 for a real failure. Collapsing both to
# success would let this guard report "passed" when it never actually ran, so
# every scan below distinguishes them explicitly.
scan() {
  scan_status=0
  scan_output="$("$@" 2>&1)" || scan_status=$?
  if [ "$scan_status" -gt 1 ]; then
    printf 'public-tree check could not run (exit %s):\n%s\n' \
      "$scan_status" "$scan_output" >&2
    exit 2
  fi
  [ "$scan_status" -eq 0 ] || scan_output=""
}

while IFS= read -r path; do
  [ -n "$path" ] || continue
  scan git grep -n -F "$path" -- \
    ':!.gitignore' \
    ':!scripts/check-public-tree.sh'
  refs="$scan_output"
  if [ -n "$refs" ]; then
    stale="${stale}${refs}
"
  fi
done <<EOF
$reference_forbidden
EOF

scan git grep -n -i -E \
  '(^|[^[:alnum:]_])(t3([ -]?code)?|happier|sessions-tmux|spotify)([^[:alnum:]_]|$)' -- \
  ':!.gitignore' \
  ':!scripts/check-public-tree.sh' \
  ':!docs/NETWORK_SECURITY.md'
external_design_refs="$scan_output"

# NETWORK_SECURITY names T3 Code only to document the shipped fleet relay's
# security boundary and the alternatives deliberately excluded from it. Keep
# every other external product-design reference forbidden in that file, and
# keep T3 forbidden everywhere else in the public tree.
scan git grep -n -i -E \
  '(^|[^[:alnum:]_])(happier|sessions-tmux|spotify)([^[:alnum:]_]|$)' -- \
  docs/NETWORK_SECURITY.md
external_design_refs="${external_design_refs}${scan_output}"

# Historical identifiers remain inside the isolated runtime compatibility
# fixture and the Go migration adapters so Sessions can recover work created
# before the rename. They are not product branding and must never leak back
# into user-facing documentation, clients, installers, or release metadata.
scan git grep -n -i -E \
  '(pretty[- _]?pty|prettygo|prettyd)' -- \
  AGENTS.md \
  ARCHITECTURE.md \
  README.md \
  ROADMAP.md \
  .github \
  Formula \
  docs \
  frontend \
  release \
  site \
  src-tauri \
  package.json \
  ':!scripts/check-public-tree.sh'
legacy_branding="$scan_output"

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

if [ -n "$legacy_branding" ]; then
  printf '%s\n' "user-facing files still contain retired product branding:" >&2
  printf '%s\n' "$legacy_branding" >&2
  exit 1
fi

node scripts/check-doc-links.mjs

printf '%s\n' "public-tree path check passed"
