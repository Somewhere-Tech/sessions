# Sessions 0.2.15

- Replaces blank native windows with an immediate startup screen and a safe
  recovery action that never stops the background daemon or live agents.
- Lets a live, idle Claude or Codex conversation open as a new Codex copy or
  fork within its current provider while the original keeps running.
- Adds `sessions fork` and matching JSON/API behavior so agents have the same
  non-destructive control as the native interface.
- Makes every reachable Fleet machine expose its complete Sessions navigator,
  so conversations retained on another Mac are not hidden behind a summary.
- Carries explicit branch context into copied conversations and excludes tool
  output, credentials, attachments, and provider-internal records.
