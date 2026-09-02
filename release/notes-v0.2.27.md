# Sessions 0.2.27

- Recognizes a provider control that a session stops at before its first turn — Claude Code's folder-trust dialog, a login prompt — as a needs-you state instead of a not-started one, so sending a message no longer presses Enter on the highlighted choice and ends the session.
- Answers those controls from the conversation view: the exact choices the terminal is showing appear as buttons, without switching to the raw terminal.
- Opens the Resume picker quickly on a large history by dropping message-count work it never displayed, lists your own conversations ahead of work started by other lanes, and closes on Escape.
- Keeps search fast while a conversation is live by indexing only the new messages appended to a transcript rather than re-reading the whole file.
- Preserves a Rich Claude or Codex conversation after its session ends; the session's own copy is no longer deleted with the runtime record.
- Attributes a Codex conversation in history by its recorded thread identity or first message rather than the nearest rollout in the same folder, and no longer reports a completed turn as failed because of a provider startup warning.
- Groups the inbox by project — a folder, a Git checkout together with its worktrees, or a Somewhere project — with the sessions waiting on you pinned above them and finished or not-connected sessions folded under each project. `sessions projects name <folder> <name>` claims a folder under a name of your choosing.
- Lets a manager see the lanes it delegated, and only those, with `sessions team` and the Lanes panel: each lane's state and its last line of work, never its whole conversation. Hand back posts a lane's result into the manager's conversation attributed to the lane, naming its branch.
- Runs delegated work on its own by default: a lane created from inside another session gets full access and its own Sessions-owned worktree on its own branch, so several lanes never trample one checkout. `--no-worktree` shares the manager's checkout; onboarding still offers "inherit my permissions" as the opt-out.
- Routes a non-autonomous Rich lane's permission requests through Sessions instead of accepting them silently (Codex) or refusing them outright (Claude): the lane waits, shows as needing you, and is answered with Allow / Allow for this session / Decline in the app, `sessions approve <id>`, or the API. The answer is recorded in the lane's transcript with who gave it.
- Treats a Rich lane that ends its turn with a question as needing you, so a blocked worker is visible from the inbox and the manager's Lanes panel.
- Restores more after a reboot: pinned sessions first, then every session you spoke to in the last day, up to eight, so a restart no longer loses the work you were in the middle of.

Existing sessions keep running across the update. No session is ended or re-adopted to install it.
