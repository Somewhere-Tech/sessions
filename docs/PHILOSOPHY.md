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

## The manager is a servant

Sessions is a session MANAGER, and managing means two things at once: total
freedom inside, total service at the interface. Internally it may reclaim a
process whenever it likes -- immediately on idle, under memory pressure, on no
schedule at all. There are no grace windows and no timing constants in
lifecycle policy, because a window is a knob for tuning how often a guess is
wrong, and the manager does not guess. Externally, the requester outranks the
machinery absolutely. A request is never wrong, never early, never late, and
never answered with an errand: "no such session, run ls", "you can resume it
if you want" -- any reply that hands work back to the requester is the servant
telling the master to go fetch, and is a defect wherever it appears, whatever
the internal state was.

Two kinds of request, two obligations. A READ -- what did it say, what is its
status, show me the work -- is served from the durable history and never needs
a process at all. A WRITE -- a new message -- wakes whatever needs waking and
delivers. Every id that ever existed resolves forever; the asking is itself
the proof that the session should exist. Even a request for a completely dead
lane is served, because the manager fetches -- always.

A requester ever discovering that a session it wanted was killed is not an
error condition. It is the product failing its founding promise, budgeted at
one in a hundred thousand, and worth telling the user to uninstall over.

## Sleep policy

- **Delegated task sessions**: aggressive, and only ever sleep. Work
  delivered and idle may mean the process goes away; the session does not.
  Nothing automatic ends a session -- the task-completion reaper that once
  did was removed, twice narrowed and still wrong, because "done" is a fact
  only the requester can know.
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
