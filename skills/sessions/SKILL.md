---
name: sessions
description: Spawn, drive, monitor, and recover long-lived Claude Code / Codex / shell sessions and headless command lanes through the local `sessions` daemon. Use when you need to run a sub-agent (e.g. dispatch a Codex agent to do a task), run a long/background command as a tracked lane, watch another session, or reliably get an agent's result without screen-scraping. Requires the sessions daemon running locally (http://localhost:8787).
---

# sessions — drive agent sessions from the CLI

`sessions` runs long-lived agent sessions and headless command lanes on this
machine, and keeps a durable record of them.

**Read `sessions help` first, then `sessions help <command>` for anything you
are about to use.** That text is the contract. It is generated from the same
source the commands dispatch from, and CI fails if the two disagree, so it
cannot describe a version of this tool that no longer exists.

This file deliberately does not repeat flags, exit codes, or output shapes.
Nothing regenerates this file and nothing diffs it against the code, so any
detail written here would eventually be a confident lie. What follows is only
the orientation you cannot get from a command list.

## What it is for

- **Work that outlives the process that started it.** A session or lane keeps
  running after you exit, are killed, or run out of context. Dispatch with
  `run`, come back later — from any process — with `wait`. Native subagents
  cannot do this; it is the main reason to reach for this tool.
- **Conversations that outlive the provider.** Providers delete their own
  transcripts on a retention timer. Sessions keeps its own copy, so reading,
  searching, and resuming still work afterwards.
- **Asking what you already did.** `search` reaches every recorded
  conversation on this machine, not just the live ones.
- **Handing work between agents.** A Claude session can drive a Codex one and
  back, with the requester recorded.

## Rules that outrank convenience

1. **Sessions are sacred.** Never kill, replace, or clean up a session you did
   not create. If you think something is stale, say so and let the user decide.
2. **Parse `--json`, and branch on the exit status.** Every command takes
   `--json`, always emits exactly one JSON document, and reports its outcome in
   the exit code as well as the body. `sessions help` lists what the codes
   mean. Do not write `if rc == 0` without reading them: several distinct
   outcomes are deliberately not success, and one of them means the target is
   never coming back.
3. **Never screen-scrape.** `snap` and `tail` are for showing a human what a
   terminal looks like. Anything you intend to act on has a structured route.
4. **If the daemon is unreachable, nothing was observed.** That is not the same
   as "the work failed". Report it as unknown.

## Check work and leave a handoff

Use `sessions team --json` to inspect all your delegates in one compact read.
Keep its `next_cursor`; the next `sessions team --since <cursor> --json`
returns only changed members. An expired or lost baseline is an error, not
evidence that nothing changed. This compares observations, not every event
between them. Do not repeatedly pull whole conversations just to check status.

When finishing delegated work, emit a verdict with `meta.handoff` describing
what changed, exact commit hashes, push status, tests and their outcomes,
artifact paths, and remaining work. Read `sessions help verdict` for the
format. These are your attributed claims, not independent verification by
Sessions. Missing evidence remains unknown. A finished turn is not proof that
the assignment is done, and a receipt does not authorize ending the runtime.

Shared-checkout warnings identify possible collisions, not ownership. Use
separate worktrees or coordinate explicitly; never reset another lane's edits.

## When something is not in the help

Ask the user. This tool changes quickly, and a plausible guess that happens to
be wrong is worse for them than a question.
