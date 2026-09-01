# Sessions 0.2.25

- Makes session titles, machine labels, and connection states shorter and easier to scan without hiding the underlying details.
- Keeps message delivery honest across reconnects and remote machines, preserving drafts whenever delivery is not confirmed.
- Lets Codex accept a follow-up while it is working and submit it after the current tool call instead of rejecting or duplicating the message.
- Keeps long conversations bounded and responsive while older history remains available on demand.
- Splits oversized frontend and runtime modules, enforces source and bundle budgets, and removes the remaining development dependency vulnerabilities.

Updating the app does not stop running sessions. Existing runners continue on their compatible bundled version until they finish.
