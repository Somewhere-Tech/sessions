# Sessions 0.2.23

- Restores accelerated macOS terminal rendering so provider pickers and other
  full-screen terminal interfaces respond immediately while retaining a narrow
  repair for explicit screen resets.
- Keeps the terminal anchored where the user intentionally scrolled while new
  output, resize, replay, reconnect, and alternate-screen changes occur; Jump
  to latest returns it to following live output.
- Keeps the hidden Terminal pane's controls behind Conversation so its
  jump-to-latest button cannot cover or intercept the Send button.
- Bounds the structured-event window held in runner and daemon memory while
  preserving complete append-only provider history on disk.
- Lets a newly updated daemon adopt older durable runners without retaining an
  unbounded replay or restarting the work they own.
- Avoids repeated socket retries for runner artifacts whose recorded process is
  definitely gone, keeping restart recovery responsive on long-lived hosts.
- Prevents login-time provider storms: same-boot runner crashes still recover,
  but a reboot restores only a bounded set of pinned roots, never repeats a
  headless lane, and preserves every paused session for explicit recovery.
- Makes delayed message delivery idempotent with durable operation receipts and
  a queryable final state, so a lost response cannot turn a retry into a
  duplicate prompt.
