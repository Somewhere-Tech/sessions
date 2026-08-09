# Sessions 0.2.17

- Keeps a session visible and recoverable while its runner reconnects instead of misreporting a temporary connection loss as a completed session.
- Makes agent-facing status and message delivery truthful, including reliable non-blocking sends and explicit attention states.
- Preserves delegated sessions until their provider has actually finished, with structured Claude helpers and unified conversation identity after resume.
- Uses one authoritative process-liveness check across recovery, cleanup, and the UI so active work is not mistaken for stale state.
- Prevents collisions between native-shell staging files during concurrent updates.
