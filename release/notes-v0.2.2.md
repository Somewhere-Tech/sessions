# Sessions 0.2.2

- Adds `sessions update`: one command checks the fixed Somewhere release feed,
  validates the exact immutable GitHub artifact path, verifies the pinned
  Minisign signature plus Developer ID and Gatekeeper, installs with an atomic
  same-disk rollback, removes the temporary backup, and reopens only the app UI.
- Keeps update discovery automatic in Sessions.app with an in-app badge and at
  most one native notification per available version. Installation remains
  explicit and sends no session data or durable device identifier.
- Scales daemon replacement readiness from a 30-second base by eight seconds
  per baseline runner, capped at five minutes. This matches serial runner
  re-adoption while preserving baseline verification and automatic rollback.
- Includes the 0.2.1 search, operations-inbox, Today, usage, fleet, pairing,
  retention, and update-handoff fixes in the signed Mac package.
