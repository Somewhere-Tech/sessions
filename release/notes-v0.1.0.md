# Sessions 0.1.0

The first macOS release of Sessions packages durable Claude Code, Codex, and
shell sessions in a native app while keeping the background service and every
runner independent from the app window.

Highlights:

- Native macOS app with tray status, scoped windows, and safe app-only quit.
- Bundled, signed Go runtime with idempotent launchd installation and rollback.
- Structured Codex conversation view with answers, progress, plans, tool calls,
  file diffs, usage context, interruption, and Terminal fallback.
- Session tags, inherited local tag defaults, and local usage analytics by
  session, tag, provider, model, and time period.
- Fleet, exact/regex/ranked search, QR device pairing, encrypted backups,
  account profiles, notifications, and worktree-aware sessions.
- Opt-in LAN and Tailscale Serve access. Sessions 0.1.0 does not use a hosted
  relay or send terminal data through a Sessions cloud service.
- Signed in-app updates that preserve the daemon and running sessions.

Sessions 0.1.0 is an Apple Silicon macOS release. Remote control is intended
for owner-controlled devices and networks; revoke lost paired devices promptly.
