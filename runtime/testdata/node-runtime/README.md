# Legacy TypeScript daemon

This directory contains the superseded Node/TypeScript daemon, runner, and CLI.
The current product runtime is implemented in `runtime/` and distributed
through Sessions.app.

Do not add product features here. The code remains temporarily because the
Go interop suite still uses this implementation as a compatibility fixture.
Its historical process names, environment variables, state paths, and launchd
labels are test inputs rather than public product branding. Remove them only
when equivalent compatibility evidence replaces the interop gate.
