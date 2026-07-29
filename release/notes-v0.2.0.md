# Sessions 0.2.0

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
- Detailed local usage reporting, automatic background refresh, provider and
  speaker filters, and query-only AI search that never sends transcripts.
- Native Connections for loopback port management, LAN, Tailscale Serve,
  one-time device pairing, encrypted Somewhere backup, and update discovery.
- Fleet machine cards, provider-aware conversation rendering, read-only
  history, safer resume behavior, Somewhere branding, and app icons.
- Hardened runtime installation, rollback, missing-agent preflight, and
  compatibility coverage for adopting durable Node-created runners without
  restarting their Claude or Codex processes.

Sessions remains local by default. Interactive browser control is deprecated;
the signed Mac app and future native mobile clients are the control surfaces.
