# Parity lane harness

Run from the repository root:

```sh
node runtime/parity/run.mjs
```

The harness builds the Go daemon/runner and the production frontend into
`runtime/parity/.scratch/`, starts the TypeScript and Go daemons on separate
ephemeral loopback ports with separate scratch homes/state, and removes the
scratch tree on exit. Its parity-owned `launchctl` shim executes each daemon's
generated runner plist without registering a real LaunchAgent.

Durable evidence is written under `runtime/parity/artifacts/`: the JSON report,
daemon/build logs, and the Go-served frontend screenshot.
