# Sessions 0.2.27

- Recognizes a provider control that a session stops at before its first turn — Claude Code's folder-trust dialog, a login prompt — as a needs-you state instead of a not-started one, so sending a message no longer presses Enter on the highlighted choice and ends the session.
- Answers those controls from the conversation view: the exact choices the terminal is showing appear as buttons, without switching to the raw terminal.
- Opens the Resume picker quickly on a large history by dropping message-count work it never displayed, lists your own conversations ahead of work started by other lanes, and closes on Escape.
- Keeps search fast while a conversation is live by indexing only the new messages appended to a transcript rather than re-reading the whole file.
- Preserves a Rich Claude or Codex conversation after its session ends; the session's own copy is no longer deleted with the runtime record.
- Attributes a Codex conversation in history by its recorded thread identity or first message rather than the nearest rollout in the same folder, and no longer reports a completed turn as failed because of a provider startup warning.

Existing sessions keep running across the update. No session is ended or re-adopted to install it.
