# Legacy TypeScript daemon

This directory contains the superseded Node/TypeScript daemon, runner, and CLI.
The current product runtime is implemented in `prettygo/` and distributed
through Sessions.app.

Do not add product features here. The code remains temporarily because the
production mini still runs the Node daemon and the Go interop/cutover suite
uses this implementation as a compatibility and rollback reference. Removal
or reduction happens only after the later joint mini cutover and observation
window.
