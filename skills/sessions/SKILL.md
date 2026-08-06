---
name: sessions
description: Spawn, drive, monitor, and recover long-lived Claude Code / Codex / shell sessions and headless command lanes through the local `sessions` daemon. Use when you need to run a sub-agent (e.g. dispatch a Codex agent to do a task), run a long/background command as a tracked lane, watch another session, or reliably get an agent's result without screen-scraping. Requires the sessions daemon running locally (http://localhost:8787).
---

# sessions — drive agent sessions from the CLI

`sessions` is a local daemon + CLI for running **long-lived sessions** (Claude Code, Codex, shell) and **headless lanes** (any command) that survive restarts, expose structured history, and can be driven and monitored programmatically. Use it to dispatch sub-agents and get trustworthy results — not by typing into a terminal and scraping the screen, but through a real contract.

**Prereq:** the daemon must be running (`sessions ls` should work). If "connection refused", it isn't running — tell the user to `sessions install` (or check `sessions doctor`). All commands accept `--json` for machine-parseable output — **always use `--json` when parsing**; never scrape `sessions snap`.

## Exit codes — read this before you write `if rc == 0`

Every waiting command reports the outcome in its **exit status**, not only in its output. The statuses are distinct on purpose, because "the thing I waited for happened", "I never found out", and "the target is never coming back" are three different situations and an agent that collapses them into success/failure will report a delegate's result that does not exist.

| code | meaning | what you should do |
|---|---|---|
| 0 | the condition was satisfied | act on the result |
| 1 | usage error (bad flags/arguments) | fix the command; do not retry it unchanged |
| 2 | the daemon could not be reached | the daemon is down — `sessions doctor`, tell the user; nothing was observed |
| 3 | timed out without observing the condition | **the target may still be working** — wait again, do not report failure |
| 4 | the target is gone or ended failed | waiting longer cannot help — report it and stop |

```bash
sessions wait "$id" --timeout 30m
case $? in
  0) sessions --json last "$id" ;;          # real result
  3) echo "still working; waiting again" ;; # NOT a failure
  4) echo "delegate died or vanished" ;;    # give up on this one
  *) echo "usage or daemon problem" ;;
esac
```

`ask` uses the same vocabulary: 0 a reply was printed, 1/2 the message was never confirmed delivered, 3 delivered but no reply within `--wait-timeout`. Under `--json`, every command emits exactly one JSON document on **every** path including failure, and its `code` field matches the exit status — so `sessions --json ... | jq` never dies on empty input.

## Core workflow: dispatch a sub-agent and get its result

```bash
# 1. Spawn. Codex runs on the structured app-server by default (reliable, no scraping).
id=$(sessions --json new --tool codex --cwd /path/to/work --name my-subtask | jq -r .id)
#   Claude:  sessions new --tool claude --cwd DIR --name NAME
#   Claude structured (subscription-billed, no live TUI): add --structured
#   Pick model/effort:  --model gpt-5.6-sol --effort high   (validated against the live catalog)

# 2. Drive it and WAIT for the reply in one call (best for request→response):
sessions ask "$id" "Do X. Reply DONE when finished."
#   `ask` = send + wait for working→idle + print the last assistant message. Claude/Codex only.

# 3. Or send + poll separately:
sessions send "$id" "your message"     # blocks until receipt is confirmed (exit 0); exit 1/2 = failed/ambiguous
sessions wait "$id" --idle 30s --timeout 30m   # block until genuinely idle for 30s
sessions --json last "$id"             # structured last user+assistant message
sessions --json status "$id"           # state / git / activity / verdict card

# 4. Clean up when done:
sessions kill "$id"
```

## Cross-provider delegation — the thing native subagents cannot do

A Claude session can create and drive a **Codex** delegate, and a Codex session can create and drive a **Claude** one. The delegate is a real session that outlives the one that created it, so you can hand off work to the other provider and collect it later.

```bash
# From a Claude session, dispatch Codex (or the reverse with --tool claude):
peer=$(sessions --json new --tool codex --cwd /repo --name second-opinion | jq -r .id)

# --from records durable, content-free attribution: the delegate sees WHICH session
# asked and can reply to it by id. Inside a Sessions session this is inherited
# automatically, so pass it explicitly only when you are not.
sessions send "$peer" --from "$SESSIONS_SESSION_ID" "Review /repo/plan.md and reply with risks."
sessions wait "$peer" --idle 30s --timeout 30m && sessions --json last "$peer"
```

`sessions resume <id> --with claude|codex` opens a linked copy of an existing conversation in the *other* provider, carrying the authored history across; the source is never modified.

## Fan-out: wait for many delegates in one call

```bash
sessions wait "$a" "$b" "$c" --all --timeout 30m   # every target; ok only if all ok
sessions wait "$a" "$b" "$c" --any --timeout 30m   # first target to finish, for a race
```

`--all` returns `{ok, kind:"all", reason, waited, results:[...]}` with one wait envelope per target **in the order you named them**, and `reason` carries the worst outcome. Sessions and lanes may be mixed. Join with one call rather than re-waiting targets one at a time — serial waits lose the delegates that died while you were blocked on an earlier one. The exit code is the worst per-target outcome, so the table above still applies.

## Headless lanes (run a command as a tracked session)

```bash
lane=$(sessions --json run --name build-check --cwd /repo -- go test ./... | jq -r .id)
sessions wait "$lane" --timeout 20m       # returns when the command exits
sessions --json last "$lane"              # exit code + output tail (completion manifest)
sessions lanes                            # list headless lanes
```

Everything after `--` belongs to the child command. Sessions' own flags (`--json`, `--name`, …) go **before** the separator; after it they are the child's.

## Track YOUR OWN lanes — do not keep a mental list

If you (an agent) are running **inside a sessions session**, every lane you spawn is automatically tagged with your session as parent. So to find what you created — **ask sessions, don't remember** (this survives context compaction):

```bash
sessions list --mine        # BOTH agent sessions AND lanes you created (this session, transitively)
sessions list --mine -a     # also include ended sessions and exited lanes
sessions list -a            # everything in every state — the one "show me everything" view
sessions lanes --mine       # just headless lanes you created
sessions list --all-owners  # every owner's records — use sparingly
```

Two independent axes, easy to confuse: **state** is `-a` (long form `--include-exited`; `--include-closed` is an old alias), **owner** is `--mine` / `--owner ID` / `--all-owners`. `--json` selects a format, not a working set: `sessions --json ls` shows what the plain table shows and still needs `-a` to include ended records.

**Rule: before ending your work, `sessions list --mine` and `sessions kill` the ones you no longer need** (this lists both agent sessions and lanes — `lanes --mine` alone misses agent sessions). Leaked lanes are the #1 orchestration failure. Never track lane ids in scratch files — query `--mine`.

## Recovery (after a crash / lost daemon)

```bash
sessions recover            # lost sessions, each with the command that actually recovers it
sessions recover --all      # also the ones that cannot be recovered, with the reason
sessions recover --reopen   # re-open every eligible unexpectedly-lost session (idempotent)
```

The RESUME column is the command to run — **run that command, do not assemble your own**. A conversation whose provider deleted its own transcript but which Sessions kept its own copy of is labelled `transcript-recovery` (`transcriptRecovery: true` under `--json`): it comes back from Sessions' copy, and a native `claude --resume` / `codex resume` on it **will be refused**. `blocked` means neither the provider nor Sessions still has it.

`sessions recover` never resurrects a session you deliberately `kill`ed (tombstoned). Use it after a reboot or if sessions vanish.

```bash
sessions transcripts            # dry run: which conversations Sessions can still copy
sessions transcripts --apply    # keep them, so a provider prune stops losing them
```

Providers prune their own transcripts on a retention timer. `transcripts` copies what is still readable into storage Sessions owns; it never moves or modifies the provider's files and is safe to re-run.

## Monitoring another session

```bash
sessions ls                       # live sessions (never lists lanes; add -a for ended ones)
sessions --json status <id>       # one session's full state
sessions --json transcript <id>   # full structured history
sessions cat <id>                 # one durable conversation, start to finish
sessions tail <id> -f             # follow output live
sessions snap <id>                # current screen (human viewing only — DON'T parse this)
```

## Sacred rules (do not violate)

1. **Never `kill` a session you did not create.** Others' sessions may be real work. Use `sessions list --mine` to know which are yours (sessions + lanes). When unsure, don't kill.
2. **Branch on the exit code, not on output alone.** `if rc == 0` treats a timeout (3) and a dead delegate (4) as the same thing as success. See the table above.
3. **Conversation collision guard:** if `sessions new`/resume refuses with "already live as ...", the conversation is being driven elsewhere — **do not `--force` past it** unless you're certain the other driver is dead. Two drivers on one conversation corrupt it.
4. **Prefer structured output.** Use `--json` and `sessions last`/`status`/`transcript`, not `snap` scraping. Codex-app-server and Claude-`--structured` sessions give authoritative done/working signals; PTY sessions are best-effort.
5. **`ask` for request→response, `send`+`wait` for fire-then-monitor.** `send` alone returns before the reply is done.
6. **Report Sessions product failures through the safe contract.** Run `sessions --json support --diagnostics`, add the sanitized failing command shape/action, exit code, expected result, and sanitized exact error, then ask the user before opening or submitting a ticket. Never attach transcripts, terminal output, paths, IDs, credentials, environment, private source, raw logs, or crash files.

## Background pattern (for long sub-tasks)

Run `sessions wait "$id" --timeout 30m &` in the background so your orchestration can be re-invoked when the sub-agent finishes, instead of blocking. Then `sessions --json last "$id"` for the result — after checking the wait's exit code.

## Quick reference

| Need | Command |
|---|---|
| spawn codex sub-agent | `sessions --json new --tool codex --cwd DIR --name NAME` |
| spawn claude sub-agent | `sessions --json new --tool claude --cwd DIR --name NAME` |
| headless command lane | `sessions run --name NAME --cwd DIR -- CMD ARGS` |
| ask + get reply | `sessions ask <id> "msg"` |
| send with attribution | `sessions send <id> --from <your-session> "msg"` |
| wait until idle | `sessions wait <id> --idle 30s --timeout 30m` |
| join many delegates | `sessions wait <id> <id> --all --timeout 30m` |
| race many delegates | `sessions wait <id> <id> --any --timeout 30m` |
| structured result | `sessions --json last <id>` / `sessions --json status <id>` |
| my sessions + lanes | `sessions list --mine` (add `-a` for ended ones) |
| everything, any state | `sessions list -a` |
| copy a conversation to the other provider | `sessions resume <id> --with claude\|codex` |
| model catalog | `sessions --json models` |
| recover lost | `sessions recover [--all] [--reopen]` |
| keep conversations past provider pruning | `sessions transcripts --apply` |
| report a Sessions problem | `sessions --json support --diagnostics` |
| clean up | `sessions kill <id>` |

Add `--host H --port P` to target a non-default daemon. `sessions --help` for everything, `sessions help <command>` for one, `sessions docs` for the complete offline reference.
