# Sessions 0.2.8

- Makes public agent launches ask first. Claude stays in the Rich structured
  experience while inheriting Claude's normal permission behavior. Codex
  starts sandboxed in Terminal until Sessions can present app-server approval
  prompts; Codex Rich now clearly requires the explicit **Full Access** choice.
  Existing saved choices and existing sessions are unchanged.
- Identifies messages relayed from one managed lane to another. Live and
  retained conversations, search, and CLI JSON/text show the source lane
  without rewriting provider history or storing relayed message content in the
  Sessions ledger.
- Adds an agent-native secure support bundle. `sessions support --bundle`
  writes a local, redacted, create-exclusive diagnostic file; attaching it to
  a Somewhere ticket is a separate explicit action that uses the user's
  existing Somewhere CLI login. Prompts, transcripts, terminal output, paths,
  identifiers, credentials, environment, raw logs, and crash files stay out.
- Moves the Windows local-host preview past its first native process-lifetime
  gate: the daemon and CLI bind to the signed-in user's Windows SID, runners
  survive the controlling client, and the merged source passes native Windows
  runtime, independent-process, shared-client, native-shell, standalone-host,
  and installer packaging checks.
- Preserves the existing conversation-first Continue, cross-machine copy,
  search, Fleet, terminal-rendering, updater, and durable runner-lifecycle
  improvements from 0.2.7.

The Windows package remains an unsigned testing preview. Authenticode,
signed-updater, normal-user hardware, provider-login, and two-user isolation
proof remain release gates before it becomes a public Windows release.
