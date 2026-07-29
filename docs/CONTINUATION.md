# Conversation continuation

Sessions treats a provider conversation as durable work and a runner as only
one way of opening it. The Continue picker can reopen the original provider
conversation or create a new conversation with the other supported agent.

## Same-provider Continue

Continuing Claude with Claude, or Codex with Codex, uses the provider's native
conversation identity. Sessions refuses to start a second writer when that
identity is already live. The ended Sessions runtime is linked to its
successor; the provider history is not copied or rewritten.

## Cross-provider Continue (preview)

Choose an earlier conversation, then choose the other agent under **Continue
with**. The CLI equivalent is:

```sh
sessions continue <history-id> --with codex
sessions continue <history-id> --with claude
```

The operation is intentionally smaller than copying provider files:

- Sessions reads the exact selected history ID.
- Only authored user and assistant text is portable. Markdown and intentional
  code blocks are retained.
- Tool calls, tool output, diffs, attachments, credentials, usage records, and
  provider-internal events are not copied.
- A new destination-provider conversation is created in the recorded
  workspace. The source conversation is never deleted or modified.
- The new Sessions runtime records the source history ID, provider, import
  mode, and imported message count.

Codex exposes an app-server history injection method. Claude-to-Codex Continue
therefore materializes authored messages as native Codex response items before
the next user turn.

Claude does not expose a supported arbitrary transcript-import method.
Codex-to-Claude Continue displays the authored source turns in Sessions and
links the exact local transcript. On the first real turn, Claude receives a
small system instruction to load that transcript through the local
`sessions --json transcript <history-id>` command. Sessions does not fabricate
Claude JSONL or claim that linked history is native Claude history.

## Long-conversation search

The source remains available to both the user and the destination agent:

```sh
sessions transcript <history-id>
sessions search "authentication decision" --session <history-id>
```

This is useful when the whole conversation should remain browsable while the
model loads only the relevant section. Search and transcript reads are local
and make no model call.

## Current limits

- Cross-provider Continue requires a complete local authored transcript.
  A prompt-only provider index is not enough.
- The preview supports Claude and Codex Rich sessions. It does not translate
  shell/PTY screen history.
- Imported bundles are local mode-0600 sidecars and are capped at 8 MiB.
- Destination-provider model context limits still apply.
- Cross-machine cross-provider continuation depends on the separate,
  user-approved conversation transfer path.
