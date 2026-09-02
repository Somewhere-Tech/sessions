# Lanes

Sessions is built for one person directing several agents at once. This
document is the model behind that: the words, the defaults, and the commands.
The behavior it describes is what the daemon and the app do today; the CLI
reference in `CLI.md` has the exact flags.

## Words

- **Project.** The work a session belongs to: a folder, a Git checkout together
  with every worktree of it, or a Somewhere project. Sessions find their
  project by working directory, so every folder shows up as an implicit
  project named after itself. `sessions projects name <folder> <name>` claims
  a folder under a name of your choosing. The inbox groups sessions by project.
- **Session.** One conversation with one agent, or one shell, kept durable by a
  runner the daemon supervises. The sessions you start yourself are the ones
  you talk to.
- **Lane.** A session created from inside another session. The session that
  created it is its manager. A lane is a durable session like any other: it
  keeps its conversation, can be opened, questioned, and ended, and it stays
  searchable after it ends.

A manager's lanes are folded under it in the inbox, with a rollup that says
how many are working and how many need you. The sessions waiting on you, lanes
included, are pinned above every project.

## How a lane starts

From inside a session, `sessions new --tool claude|codex "<request>"` starts a
lane with that request as its first message. `sessions fanout -- <request>`
starts one lane per installed provider with the same request and joins them,
so a change can be checked by an agent from each provider in one step. From
the app, New Session with a parent chosen does the same.

Two defaults apply to a lane and not to a session you start yourself:

- **It runs on its own.** A lane has full access, because delegated work runs
  in the background and is expected to finish rather than wait on a person for
  each command. The machine-level opt-out in Settings makes new lanes inherit
  their manager's permissions instead; see Asking below.
- **It works in its own worktree.** When the lane's folder is a usable Git
  checkout, the daemon creates a Sessions-owned worktree on a branch of its
  own, so several lanes never trample one checkout and the manager's folder
  stays as the person left it. `--no-worktree` shares the manager's checkout.
  A folder that cannot host a worktree is shared, with the reason logged.

The daemon enforces that a lane can never widen its own access past what the
machine allows.

## Asking

A session you start with **Ask me** (or a lane under the inherit opt-out) asks
before it runs a command, changes files, or takes more access. For a Rich
Claude or Codex session the request does not stop at a terminal prompt: the
runner holds it open, the session reads as needing you with a line such as
"Allow? Run `npm test`", and it is answered from the conversation view, from a
lane row in the Lanes panel, with `sessions approve <id> [--deny |
--for-session]`, or over the API. The answer is recorded in the session's
transcript with who gave it, `sessions cat <id>` shows both the request and the
answer as approval audit records, and a manager lane can answer for its own
lanes. For Codex, **Ask me** uses the provider's untrusted approval policy in a
workspace-write sandbox, so ordinary commands ask instead of proceeding under
the on-request policy. Nothing is approved on silence: a request still open
when the turn is cancelled or the runner stops is denied.

Your own Claude allow and deny rules still apply first, so an asking session
prompts only for what you would have been asked yourself.

## What a lane can see

A lane sees the lanes it is responsible for and its own manager, never other
projects. `sessions team` lists a manager's parent and its delegated
descendants with a compact state and the last line of work, under a hard
size budget per row, so a manager can watch its workers without pulling their
conversations into context. `sessions team --all` is the view from the top:
every session that has delegated lanes with its lane, working, and needs-you
counts, and the waiting lanes named.

A lane's full conversation is an explicit read (`sessions cat <id>`), never
something a listing carries.

## Handing back

Work returns to the manager by a hand-back. In the app, the Lanes panel and
the lane's own view both offer **Hand back**: it posts the lane's latest line
into the manager's conversation, attributed to the lane, together with the
branch and worktree its work is on, and returns you to the manager. The lane
keeps running; a hand-back is a report, not an end.

When a lane ends, its branch and worktree are kept. `sessions worktrees`
lists them with their merge state, and `sessions worktrees clean` removes only
worktrees whose session has ended, whose tree is clean, and whose branch is
fully merged. Sessions never cleans a worktree on its own.

## Not losing a session

Every session is a conversation first and a process second. After a reboot
the daemon restarts at most eight sessions on its own, pinned ones first and
then the ones you spoke to in the last day. The rest stay paused, listed
under "Not connected" with the reason, and wake on first contact: opening one,
sending it a message, or reading it live restarts its runner in place with
the same id, conversation, and folder. A session whose runner is gone for
good is resumed from its conversation with `sessions resume`.

## Commands in one place

| Task | Command |
| --- | --- |
| Start a lane from inside a session | `sessions new --tool codex "<request>"` |
| One lane per provider, joined | `sessions fanout -- <request>` |
| What am I responsible for | `sessions team` |
| Everything waiting on me, across managers | `sessions team --all` |
| Answer a lane's question | `sessions ask <id>` |
| Allow or decline a lane's permission request | `sessions approve <id> [--deny \| --for-session]` |
| Read a lane's conversation on purpose | `sessions cat <id>` |
| Name a folder as a project | `sessions projects name <folder> <name>` |
| Kept worktrees, and cleaning the merged ones | `sessions worktrees`, `sessions worktrees clean` |
