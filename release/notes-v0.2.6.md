# Sessions 0.2.6

- Rebuilds Sessions as a clear operations inbox: live work stays under Running,
  ended work is grouped by outcome, and every session has explicit View,
  Close view, End, Resume, Move, and Archive actions where applicable.
- Makes termination auditable. New end records preserve the initiating local
  agent or paired device, exact time, client, optional literal reason, and batch
  operation. Multi-session termination is preflighted and protected by the
  aggregate mass-kill guard.
- Makes Resume a first-class, linked lifecycle action. The old runtime remains
  immutable, the new runtime points back to it, and titles, tags, account
  profile, hierarchy, provider identity, and Rich/Terminal mode carry forward.
- Adds explicit Rich and Terminal choices. Codex defaults to its app-server
  conversation runtime; Claude remains terminal-first with a clearly labelled
  structured preview. Active Claude turns reject extra input visibly instead
  of creating a hidden prompt queue.
- Improves search and history readability with stable query state, clearer
  user-versus-agent results, provider identity, message timestamps, read-only
  View language, and safer ANSI/control-message cleanup.
- Keeps degraded sessions live and honest: optional MCP failures render as
  Limited rather than crashed, while lost runners remain visible during
  bounded recovery.
- Adds provider-version inspection and one-click update surfaces, accessible
  ended-session selection/archive, larger readable typography, Fleet polish,
  and the responsive mobile foundation used by the Android client.
