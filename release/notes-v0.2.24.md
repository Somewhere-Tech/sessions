# Sessions 0.2.24

- Restores both sides of terminal-backed Codex conversations after a Sessions daemon or app restart by durably binding the exact provider conversation.
- Labels old PID-less runner records as **Connection lost** instead of promising an endless reconnection that cannot occur.
- Keeps live sessions sendable when only the display stream is reconnecting; drafts remain intact when delivery really is unavailable.
- Lets an authorized paired Sessions client update Claude or Codex on the selected computer and names that target before the update runs.

Updating the app does not stop running sessions. Existing runners continue on their compatible bundled version until they finish.
