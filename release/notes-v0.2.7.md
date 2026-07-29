# Sessions 0.2.7

- Makes Continue a first-class Sessions capability: Claude and Codex history is
  grouped by provider conversation instead of duplicated for every runtime,
  with linked continuation history and stable conversation titles.
- Lets retained Claude prompt-index conversations continue only when Sessions
  can prove the exact provider identity and recorded workspace. It refuses to
  guess another chat or imply that a missing local transcript was preserved.
- Adds an ended-only native **Continue on another machine** flow using saved
  device credentials, a mandatory dry-run review, source-history preservation,
  create-exclusive destination writes, and linked source/target provenance.
- Defaults new Claude and Codex sessions to the structured Rich experience.
  Terminal remains an explicit compatibility choice and existing sessions keep
  the mode in which they were started.
- Improves conversation-first search and terminal rendering: provider-native
  titles stay visible, results remain grouped by conversation, and full-screen
  terminal updates no longer leave unreadable ghosted cells.
- Makes Fleet a calm glance surface: machine cards stack before becoming
  cramped, Windows/macOS/Linux identity is explicit, version differences stay
  readable, and long session context no longer forces status labels to wrap.
- Adds the source-complete Windows host foundation: per-user supervision,
  immutable Go runtimes, secured Named Pipes, ConPTY, Job Objects, protected
  machine credentials, and an installed `sessions` CLI.
- Gives Windows the same background update check, sidebar notification,
  signature verification, current-user installer, and Fleet version tracking
  contract as the Mac app. Authenticode and real-hardware update proof remain
  release gates.
- Preserves the existing lifecycle and conversation polish: clear running and
  ended work, first-class Continue, explicit copy actions, model/effort controls,
  attributable termination, working-set organization, and readable provider
  and machine identity.
