# Conversation continuation

Sessions treats a provider conversation as durable work and a runner as only
one way of opening it. The Continue picker can reopen the original provider
conversation or create a new conversation with the other supported agent.

## Fork a conversation

You do not have to end a live Claude or Codex chat to branch it. From the live
session menu, choose **Fork a copy in Claude/Codex** for the same provider or
**Open a copy in Claude/Codex** for the other provider. Sessions then shows the
same start review used everywhere else: agent, model, effort, Rich runtime,
**Ask me** access, and the message count and estimated size of the copy. Nothing
runs until you press **Start**, whose label names the chosen agent and model.
The CLI equivalent is:

```sh
sessions fork <live-session-id>
sessions fork <live-session-id> --with codex
sessions fork <live-session-id> --with claude
```

Sessions waits until a live source's current turn is finished, takes one stable
authored-history snapshot, and creates a new conversation beneath the source in
the session tree. The original runtime, provider conversation, and working
state stay unchanged. This is a branch, not a resume: it never marks the source
as superseded and never needs a force flag.

The review says exactly what crosses the boundary. Your messages and the
agent's replies are copied. Tool output, file changes, attachments, sign-in
details, usage totals, and the agent's behind-the-scenes records stay out.

From saved Conversation history, choose **Fork from here** beneath any user or
agent message. Sessions records that exact normalized message index and stable
message ID, copies authored history only through that point, and refuses the
operation if the selected message changed after it was displayed. CLI callers
can use the same guarded boundary:

```sh
sessions fork <session-id> --at 42 --message-id <stable-message-id>
```

## Same-provider Continue

Continuing Claude with Claude, or Codex with Codex, uses the provider's native
conversation identity. Sessions refuses to start a second writer when that
identity is already live. The ended Sessions runtime is linked to its
successor; the provider history is not copied or rewritten.

Before the successor starts, the app shows a compact plan with the resolved
agent, provider-default model, effort, Rich or Terminal runtime, and **Ask me**
access. The model, effort, and runtime can be reviewed or changed there. Because
this is a native resume, there is no transfer-size claim: the provider reopens
its own history. Nothing runs until the person presses **Start**, and the button
names the chosen agent and model.

## Continue a conversation with a different agent

Choose an earlier conversation, then choose the other agent under **Continue
with**. Before anything starts, Sessions shows:

- the conversation name, message count, and an approximate token count;
- the agent, model, effort, Rich runtime, and **Ask me** access that will receive
  the history;
- a choice between the whole conversation and only the last N messages; and
- a reminder that the original conversation will not change and nothing runs
  until you press **Start**. The Start button names the chosen agent and model.

Sessions estimates one token for every four characters. This is a warning
estimate, not the destination provider's bill. If the whole history is above
60,000 estimated tokens, **Start** stays disabled until you either choose a
shorter tail or confirm **Send the whole history anyway**.

After Start, the dialog reports each step: exporting the history, creating the
new session, starting the chosen agent, and waiting for its first reply. If the
agent has not replied after a minute, you can keep waiting or cancel. Cancel
ends only the newly created session; it does not change the original. Provider
sign-in, quota, and connection problems appear in the same dialog.

When the first reply arrives, Sessions opens the new conversation. Its first
system line names the source, source agent, number of messages, and chosen
model so it is always clear how the conversation began.

The CLI equivalent is:

```sh
sessions continue <history-id> --with codex
sessions continue <history-id> --with claude
```

The history transfer is intentionally smaller than copying provider files:

- Sessions reads the exact selected history ID.
- Only your messages and the agent's replies are portable. Markdown and
  intentional code blocks are retained.
- Tool calls, tool output, file changes, attachments, sign-in details, usage
  totals, and the agent's behind-the-scenes records are not copied.
- A new destination-provider conversation is created in the recorded
  workspace. The source conversation is never deleted or modified.
- The new Sessions session records the source history ID, provider, transfer
  method, and message count.

Sessions gives the selected messages to the new agent as conversation context;
it does not alter or pretend to extend the original provider conversation.

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
- A live source must be idle so Sessions never copies a partial assistant
  response.
- The preview supports Claude and Codex Rich sessions. It does not translate
  shell/PTY screen history.
- Imported bundles are local mode-0600 sidecars and are capped at 32 MiB.
- Destination-provider model context limits still apply.
- Cross-machine cross-provider continuation depends on the separate,
  user-approved conversation transfer path.
