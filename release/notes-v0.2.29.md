# Sessions 0.2.29

This release consolidates completed conversation, account, recovery, and
performance changes. Source integration, signed artifacts, publication, and
installed apps remain distinct states. These notes do not assert that a user's
computers have been updated.

## Changes

- Project-first navigation keeps delegated work reachable, including on narrow
  screens. Conversation headers, account selection, and routine status displays
  are calmer and easier to read across themes.
- Claude next-turn messages are saved and acknowledged. Paste-envelope user
  instructions remain visible in history and can confirm delivery. Unknown
  delivery is never an instruction to send the same message again.
- Codex steering retains uncertain submissions with explicit unconfirmed labels
  and preserves submission order. Approvals remain responsive during provider
  work; interruption requests do not resend queued follow-ups.
- Delegated children inherit their manager's account when using the same
  provider, unless an account or the default profile is explicitly selected.
  Fresh account checks supersede stale identity and usage observations. Sign-in
  helpers find provider CLIs outside a GUI service's inherited PATH.
- Session-list reads fail truthfully when interrupted rather than claiming an
  empty list. Cached reads, runner controls, resume lookup, transcript buffers,
  and activity-only UI caching avoid repeated expensive work.
- `sessions read` offers bounded, independently replayable conversation pages.
  Callers retain a cursor to read newly appended messages; replacement or
  compaction is reported rather than silently resetting their position.
- Codex history lookup recognizes provider UUIDs and selects duplicate rollout
  sources using recorded activity instead of copied filesystem timestamps.
- Linux login-service installation and removal report observed outcomes and
  scope removal to Sessions' generated integration. Removing that integration
  does not stop existing session processes.
- Partial history appends, first-prompt recovery, source-machine restart review,
  notification subscription visibility, and native permission guidance are more
  explicit. Existing runners remain independent of clients and the daemon.

## Verification and distribution boundaries

Source and fixture tests do not replace provider, native-device, or exact-byte
release acceptance. Linux native CI execution is separate from real systemd
login/reboot and GUI acceptance. Windows packaging and physical phone journeys
require their own checks. Optional hosted services are not deployed by this
release, and experimental provider/authentication branches are not enabled by
this consolidation.

Public macOS artifacts require nested Developer ID signatures, notarization,
stapling, Gatekeeper checks, and verified updater identity. Compatible live
runners must survive install/update acceptance. npm publication and hosted
updater or website promotion remain separate verified delivery steps.
