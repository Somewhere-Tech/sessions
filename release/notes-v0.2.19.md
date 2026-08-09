# Sessions 0.2.19

- Distinguishes human messages from agent and provider activity so working,
  attention, and engagement states reflect what actually happened.
- Makes End terminate the complete process tree owned by a session while
  preserving deliberately detached work and independently managed services.
- Serializes concurrent metadata updates so names, tags, relationships, pins,
  and runner state do not overwrite one another.
- Preserves and mirrors substantially larger Claude and Codex histories for
  reliable search, recovery, and continuation.
- Updates the desktop dependency baseline and ships with no known npm audit
  vulnerabilities.
