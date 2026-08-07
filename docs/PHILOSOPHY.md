# Session lifecycle philosophy

A session is a conversation with a workspace, not a process. The process is
cache. This page is the reference for every lifecycle, listing, and search
decision; when a design conflicts with it, the design is wrong or this page
gets amended deliberately.

## Three states

**AWAKE** — a provider process is running.

**ASLEEP** — no process. The conversation is durable (transcript plus
Sessions' own mirror), the session appears in every listing marked asleep, and
sending to it wakes it. Sleep is invisible to the sender: the message is held,
the provider is started against its conversation, delivery waits for observed
readiness and for any resume decoration (pickers, banners) to clear, and the
message is delivered — confirmed delivered only when it appears in the
transcript, never because bytes were pasted. Anyone's message wakes a sleeper;
Sessions holds the message and makes the sender's assumption true rather than
bouncing them for asking.

**ENDED** — a deliberate archive state, and the only state the word "resume"
applies to. A session ends exactly two ways: the user ends it, or it stays
asleep past the retention window without being pinned. Ended conversations
remain searchable and readable forever; reopening one is an explicit act.

There is no other state. "Finished", "exited", "failed at startup", and the
rest are diagnostics, not lifecycles, and must not appear as top-level session
states in any listing.

## Sleep policy

- **Delegated task sessions**: aggressive. Work delivered and idle means
  sleep. A subagent is a function call with memory; a function that returned
  does not hold a process.
- **User sessions**: gentle. Roughly a day of inactivity before sleeping.
- **Pinned**: never sleeps automatically and never auto-ends. Pinning is the
  user marking a workbench, and the machinery keeps its hands off it.

"Idle" and "done" are inferred from the outside and the inference is
sometimes wrong. That is acceptable for sleep and unacceptable for kill: the
penalty for a wrong guess must be a nap, not a death. Nothing may kill a
session because a classifier believed it was finished; classifiers may only
put sessions to sleep, where a wrong guess costs one wake latency.

## Visibility

Asleep sessions stay first-class everywhere: in `sessions ls`, in a parent's
view of its delegates, in counts. Sleep exists precisely so that reclaiming a
process does not make work disappear. If reclaiming a session removes it from
any listing an agent or user consults, that is a kill wearing sleep's clothes.

## Whose session is it

Search and default listings scope to sessions a **human** actually spoke
into. A parent agent driving its child through the input routes is not the
human; every input path records its principal so the two are
distinguishable. Agent-only sessions are one toggle away, never gone.

## The second axis

The other grouping is the project: workspace, git repository, and (where it
applies) the somewhere project it deploys to — with "multiple" and "none" as
honest buckets, because agents genuinely work across several and sometimes in
none.
