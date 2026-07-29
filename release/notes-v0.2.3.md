# Sessions 0.2.3

- Corrects the native daemon-upgrade readiness budget using the full observed
  runner adoption cost: HELLO plus the initial replay window. Readiness starts
  at 30 seconds, adds 15 seconds per baseline runner, and remains capped at
  five minutes.
- Preserves the same fail-closed upgrade contract: the app rolls only the
  daemon back if health, discovery, or any baseline session is missing; it
  never stops or replaces a runner.
- Includes the secure `sessions update` command and automatic update-available
  notifications introduced in 0.2.2.
