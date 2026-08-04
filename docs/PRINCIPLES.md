# Product principles

Sessions is a local-first operations surface for durable Claude Code, Codex,
shell, and command sessions. These principles describe the public product
contract. Internal decision logs, launch notes, rejected options, and private
service plans are maintained outside this repository.

## Sessions are durable work

A window is not a process and a process is not a conversation. Closing a tab
must not silently end work. Ending a runtime must be explicit, and provider
history remains available for supported continuation and search.

## Users and agents are equal operators

Operational controls should have a stable CLI and JSON contract as well as a
clear native UI. An agent may inspect and operate Sessions with the user's
authority, but it does not bypass approvals, credentials, or destructive-action
boundaries.

Delegation is explicit authority, not a loophole. A child inherits its
manager's exact provider permission mode by default and cannot promote itself.
The user may opt into autonomous delegated work at the machine level, with that
choice visible in Settings and applied only to newly created children. Provider
approval prompts are durable `needs-input` state for users and agents; Sessions
does not clear them by blindly typing into a terminal.

Short-lived agent workers should finish without becoming clutter. An
agent-created task runtime may close after a successful final response, while
its transcript, lineage, workspace, and usage remain durable. User-led sessions
and explicitly long-lived manager sessions remain open until somebody ends
them. A failed or blocked task stays visible because cleanup must never hide an
unresolved decision.

## History is an agent-native memory layer

Sessions is not only a window manager. It is the durable index over Claude and
Codex conversations that lets a user or agent find prior work without knowing
which provider or approved machine holds it. Fleet search returns stable,
machine-qualified references; those same references can be read or continued
through the CLI and JSON contract. A temporarily offline machine is reported as
partial coverage, never silently presented as an empty history.

Search does not create a second transcript store. Provider history stays on its
source machine until the user opts into backup or explicitly moves work. When a
conversation is continued, Sessions preserves the source and records the new
runtime as linked history. “Resurrect” means reconstructing a supported
conversation from durable provider history—not pretending to restore process
memory or uncommitted filesystem state.

Provider helper and subagent messages remain searchable when they are present in
the provider's durable conversation. Search must keep their authorship distinct
from the person's messages so callers can filter or interpret them without
pretending every provider-side event came directly from the user.

## Local by default

The daemon listens on loopback by default. LAN, tailnet, backup, notification,
and hosted features are opt-in and must explain what leaves the machine.
Sessions makes no hidden model call and does not require a Sessions account for
local operation.

## Direct connections before relays

Native clients connect directly to a user-controlled Sessions host on localhost,
a trusted LAN, or a user-owned tailnet. A hosted service must be separately
identified and must not silently turn into a tunnel to a user's local machine.

## Calm, literal lifecycle language

Routine states such as finished, resumable, archived, or temporarily offline
are not emergencies. Red and blocking confirmation are reserved for meaningful
danger or irreversible loss. Labels such as Close tab, Set aside, End session, Continue,
Move, and Archive must describe their actual effect.

## Compatibility over forced lockstep

The app, daemon, CLI, and runners have separate lifetimes. Compatible versions
may cooperate; an update must not replace a live runner or discard an open
session. Protocol ranges, not display-version equality, determine compatibility.

## Open local runtime, explicit hosted boundary

The code that runs on a user's device, its local formats, and its versioned
protocols are buildable from this repository. Optional Somewhere account,
backup, relay, billing, abuse-control, and hosted-worker services may live in
private repositories and consume those public contracts. See
[`OPEN_SOURCE_BOUNDARY.md`](OPEN_SOURCE_BOUNDARY.md).
