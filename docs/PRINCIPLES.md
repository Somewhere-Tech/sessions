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

Agent-created children are durable sessions by default. A final response is not
proof that the caller is done with the runtime: the child may own a server,
watcher, follow-up context, or work the manager intends to revisit. A caller may
explicitly declare a bounded `task`, but Sessions never infers permission to end
one merely because its latest turn looks complete. Cleanup reduces clutter by
grouping or archiving durable records; it must not hide unresolved decisions or
silently terminate work.

Agent-created work should use the provider's structured boundary when that
boundary preserves the user's subscription, authority, and required controls.
Claude children therefore default to structured provider events, with an
explicit terminal escape hatch. Sessions does not force Codex children onto its
app-server while doing so would hide constrained approval prompts or require a
silent permission escalation.

## History is an agent-native memory layer

Sessions is not only a window manager. It is the durable index over Claude and
Codex conversations that lets a user or agent find prior work without knowing
which provider or approved machine holds it. Fleet search returns stable,
machine-qualified references; those same references can be read or resumed
through the CLI and JSON contract. A temporarily offline machine is reported as
partial coverage, never silently presented as an empty history.

Provider history is first-class Sessions history whether or not Sessions
started the conversation. Opening Sessions on an existing machine should
immediately discover the user's available Claude and Codex conversations—there
is no migration ceremony and no requirement to have launched them through the
Sessions UI or CLI. A conversation started in a provider app, terminal, or
another compatible client must still be searchable, readable, organizable, and
resumable from Sessions whenever its provider history remains available.

Starting work through Sessions is the enhanced path, not the admission price.
Sessions-launched work can carry richer live status, ownership, parent/child
lineage, machine identity, grouping, movement, recovery, and usage metadata.
Those additions should make Sessions more useful from the first new session
without making older or externally opened conversations second-class. The
product's time-to-value starts with the history the user already has.

Search does not create a second transcript store. Provider history stays on its
source machine until the user opts into backup or explicitly moves work. When a
conversation is resumed, Sessions preserves the source and records the new
runtime as linked history. “Resurrect” means reconstructing a supported
conversation from durable provider history—not pretending to restore process
memory or uncommitted filesystem state.

Provider helper and subagent messages remain searchable when they are present in
the provider's durable conversation. Search must keep their authorship distinct
from the person's messages so callers can filter or interpret them without
pretending every provider-side event came directly from the user.

Agent-created child sessions recede beneath the manager that delegated them.
The manager shows a compact status rollup; a dedicated Subagents panel reveals
their purpose, state, and conversation only when requested. A person can make
one a main session without rewriting its trusted creator history or restarting
its runtime. Needs-input counts remain visible even while the child rows are
collapsed, so quieter navigation never hides a decision.

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
danger or irreversible loss. Labels such as Close tab, Set aside, End session,
Resume, Move, and Archive must describe their actual effect.

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
