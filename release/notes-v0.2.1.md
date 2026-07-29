# Sessions 0.2.1

Sessions 0.2 turns the native Mac app into an agent operations inbox while
preserving the independent daemon and every durable runner.

Highlights:

- A redesigned Sessions workspace with a global rail, lineage-aware session
  navigator, pinned managers, nested children, open-session tabs, and separate
  Conversation, Terminal, and Details modes.
- Task-first New Session and Delegate flows with provider profiles, inherited
  context, typed Claude launch defaults, and no hidden prompt queue.
- A local Today journal that combines Sessions lanes with observed Claude and
  Codex work, plus an optional compact recap through the user's authenticated
  Codex or Claude CLI.
- Detailed local usage reporting and automatic background refresh.
- Full-history search validated against a large Claude manager lane: broad
  ranked recall, provider/speaker/operations/date/workspace filters, exact
  message bookmarks, typed delegation and handoff events, and paged read-only
  context/range/full-history views. Query-only AI planning never sends
  transcripts.
- Native Connections for loopback port management, LAN, Tailscale Serve,
  one-time device pairing, encrypted Somewhere backup, and update discovery.
- Fleet machine cards, provider-aware conversation rendering, read-only
  history, safer resume behavior, Somewhere branding, and app icons.
- Hardened runtime installation, rollback, missing-agent preflight, and
  compatibility coverage for adopting durable Node-created runners without
  restarting their Claude or Codex processes.
- Update-handoff follow-ups: truthful doctor/version reporting, safe
  dry-run-first ledger retention, honest per-lane Git deltas, and managed CLI
  link repair.

Sessions remains local by default. Interactive browser control is deprecated;
the signed Mac app and future native mobile clients are the control surfaces.
No session data or telemetry is sent to Somewhere unless the user explicitly
enables a data-bearing integration.
