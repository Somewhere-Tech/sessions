# Sessions 0.2.21

- Delivers the first request of a new Claude or Codex session only after the provider has actually produced its ready screen, preventing cold-start messages from disappearing during terminal initialization.
- Keeps a live Claude session bound to its exact provider conversation instead of briefly importing an older transcript merely because it is the only history file in the selected folder.
- Prevents rapid Enter/click gestures from creating duplicate sessions or sending the same message twice.
- Makes typing in Claude's macOS terminal view responsive by moving the full-screen artifact repair repaint to the end of each output burst.
- Accepts the daemon's leading `v` when verifying a completed update, so a successful install is not falsely reported as a version mismatch.
