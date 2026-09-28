# Sessions from npm

This package installs the native Go CLI, daemon, and runner. Node.js 20 or newer
is only the installer and command launcher; providers still run under the Go
runtime. Supported release targets are macOS ARM64, Linux AMD64, and Linux
ARM64. WSL2 uses the Linux archive; service startup depends on its systemd
configuration and needs separate acceptance. Native Windows and Intel macOS are not
included in this npm package.

After this package is published, install the exact release:

```sh
npm install --global @somewhere-tech/sessions@0.2.27
sessions help
sessions install
sessions status --json
```

`npm install` downloads the platform archive over HTTPS and verifies the SHA-256
archive and individual binary hashes pinned inside the npm package. It never
installs a service, starts a daemon, ends a session, or changes permissions.
`sessions install` is the explicit service setup step; read `sessions help
install` for your platform's options. Installing a Linux package is separate
from proving service startup, restart, and reboot recovery on that machine.

If your npm policy uses `--ignore-scripts`, the first command stages the same
verified runtime. First staging requires access to GitHub release downloads.
Once staged, commands verify and use the local binaries without network access.
Downloads have a 128 MiB compressed and 256 MiB expanded limit. Unsupported
architectures, unavailable release assets, failed TLS, and checksum mismatches
fail with an error instead of using an unverified fallback.

## Updates and removal

Use `npm install --global @somewhere-tech/sessions@VERSION` to select a release.
Each version uses immutable bytes under
`~/.local/share/sessions/npm/<version>-<platform>-<archive-sha256>/`. This path
is outside `node_modules`, so npm updates and removal leave the original daemon
and runners' executable paths available. Updating npm does not restart an
existing daemon; follow the platform's explicit runtime upgrade instructions.

`npm uninstall --global @somewhere-tech/sessions` removes command launchers.
It does not remove services, state, transcripts, or cached runtimes. Keep cached
runtime directories while any daemon, runner, or service definition uses them.
Cache corruption fails verification; inspect the exact path in the error before
removing an unused version and reinstalling it.

## Preparing a release

The checked-in 0.2.27 manifest is intentionally unprepared until the exact
tested runtime archives exist. It cannot install or pass the publication gate.
Do not label 0.2.26 binaries as a 0.2.27 npm release.

Prepare a copy of `npm/` for release, with its version matching the tested
runtime archives and their `.sha256` files:

```sh
node npm/scripts/prepare-release.cjs /path/to/verified/cli /path/to/npm-package
npm --prefix /path/to/npm-package test
npm pack /path/to/npm-package
```

The preparer requires all three supported platform archives, verifies their
checksum files, rejects unexpected archive members, and pins the binary hashes.
`npm publish` also fetches every official versioned GitHub asset and verifies
those exact bytes against the manifest before allowing publication. Tests must
include the packed npm artifact and operational platform acceptance separately.
Publishing requires an npm identity authorized for the `@somewhere-tech` scope.
Use an npm trusted publishing job with only npm publication authority, after
the tested release assets are published; do not share application signing keys
with npm dependency installation or test jobs.
