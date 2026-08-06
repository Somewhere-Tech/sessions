# Install Sessions

Sessions.app is the primary macOS package, carrying a bundled-runtime installer
and a signed updater. Public releases are Developer ID signed, notarized,
stapled, and backed by an immutable updater artifact; see
[GitHub Releases](https://github.com/somewhere-tech/sessions/releases/latest)
for the current version. The standalone instructions below remain useful for
agents, developers, and headless installs. Do not use them to change the
production mini.

The standalone runtime ships as three static Go binaries:

- `sessions` — CLI
- `sessionsd` — daemon and embedded web UI
- `sessions-runner` — one long-lived PTY owner per session

Keep all three in the same directory. Sessions uses that adjacency to locate the
daemon and runner. Node, npm, and the retired repository install script are not
required.

For local native-app development and the public release gate, use
[`NATIVE_APP.md`](NATIVE_APP.md) and [`RELEASE.md`](RELEASE.md).

## Requirements

- macOS arm64 (Apple Silicon), Linux arm64, or Linux amd64
- Claude Code and/or Codex installed separately if you plan to run those tools
- Tailscale on both devices only if you enable early-access remote access

## Native macOS app

The release page provides `Sessions_<version>_darwin_arm64.zip` for the first
install and `Sessions.app.tar.gz` plus its updater signature for the in-app
updater. The app zip is signed, notarized, stapled, and checked by Gatekeeper in
CI before the GitHub Release becomes visible. Extract it, move `Sessions.app` to
Applications, and open it normally. First run installs or adopts the independent
daemon; quitting the app does not end the daemon or any session.

Install the native app through Homebrew with:

```sh
brew install --cask somewhere-tech/tap/sessions-app
```

## Windows host preview

The Windows preview release provides two packages:

- `Sessions_<version>_x64-setup.exe` — the normal current-user installer;
- `Sessions_<version>_x64-portable.zip` — a standalone test layout.

Prefer the installer. A newer installer is the normal upgrade path for an
existing installed copy of Sessions; do not delete `%LOCALAPPDATA%\Sessions`
or the Sessions state directory during an upgrade. The updater and installer
are designed to leave compatible runners on their existing runtime while new
sessions use the updated runtime. This preservation contract still requires
the real-hardware checks in [`WINDOWS_TEST.md`](WINDOWS_TEST.md) before the
unsigned preview can be described as production-ready.

The portable package is not registered as an installed application and cannot
update an older portable copy in place. Close the portable viewer, remove its
old extracted directory, and extract the new archive to a fresh directory.
Provider credentials and Sessions state live outside that directory.

The current preview installer is intentionally unsigned. Windows may show a
SmartScreen warning until Authenticode signing and hardware verification are
complete.

## Homebrew runtime install on macOS

Homebrew is the npm-like package-manager channel for the three standalone
runtime binaries:

```sh
brew install somewhere-tech/tap/sessions
sessions install
open http://localhost:8787
```

The public `somewhere-tech/homebrew-tap` repository pins immutable release URLs
and their SHA-256 digests. It installs native binaries directly; Node and npm
are not runtime dependencies.

`sessions install` writes
`~/Library/LaunchAgents/tech.somewhere.sessions.dev.daemon.plist`, starts the per-user
daemon, waits for `http://127.0.0.1:8787/api/health`, and prints the auth token.
The label defaults to the collision-safe development value above and can be
configured with `SESSIONS_DAEMON_LABEL`. The generated plist always includes
the selected host/port and the absolute adjacent `sessions-runner` path. It does not
install or modify Claude Code, Codex, or Tailscale.

## Static archive and autonomous agent install

Release assets use these names:

| Platform | Archive |
| --- | --- |
| Apple Silicon macOS | `sessions_<version>_darwin_arm64.tar.gz` |
| arm64 Linux | `sessions_<version>_linux_arm64.tar.gz` |
| amd64 Linux | `sessions_<version>_linux_amd64.tar.gz` |

Set a release version without the leading `v`, select the archive, and download
it directly from GitHub Releases. This example is for Apple Silicon macOS:

With GitHub CLI, agents can select an immutable tag without parsing a web page.
`gh` uses the agent's existing GitHub authentication while the repository is
private; no repository checkout, npm, Node, or install script is involved:

```sh
# Resolve the current release, then pin it for the rest of the install.
VERSION="$(gh release view --repo somewhere-tech/sessions --json tagName -q .tagName)"
VERSION="${VERSION#v}"
ARCHIVE="sessions_${VERSION}_darwin_arm64.tar.gz"
DOWNLOAD_DIR="$(mktemp -d)"
gh release download "v${VERSION}" --repo somewhere-tech/sessions \
  --pattern "$ARCHIVE" --pattern "$ARCHIVE.sha256" \
  --dir "$DOWNLOAD_DIR"
(cd "$DOWNLOAD_DIR" && shasum -a 256 -c "$ARCHIVE.sha256")
tar -xzf "$DOWNLOAD_DIR/$ARCHIVE" -C "$DOWNLOAD_DIR"
mkdir -p "$HOME/.local/bin"
install -m 0755 \
  "$DOWNLOAD_DIR/sessions" \
  "$DOWNLOAD_DIR/sessionsd" \
  "$DOWNLOAD_DIR/sessions-runner" \
  "$HOME/.local/bin/"
```

For a public repository, the same command works without authentication. Agents
that do not have `gh` can use the direct HTTPS form:

```sh
# Substitute the version you intend to install. The releases page always shows
# the current one: https://github.com/somewhere-tech/sessions/releases/latest
VERSION=0.2.16
ARCHIVE="sessions_${VERSION}_darwin_arm64.tar.gz"
curl -fLO "https://github.com/somewhere-tech/sessions/releases/download/v${VERSION}/${ARCHIVE}"
curl -fLO "https://github.com/somewhere-tech/sessions/releases/download/v${VERSION}/${ARCHIVE}.sha256"
shasum -a 256 -c "${ARCHIVE}.sha256"
tar -xzf "$ARCHIVE"
mkdir -p "$HOME/.local/bin"
install -m 0755 sessions sessionsd sessions-runner "$HOME/.local/bin/"
```

Linux users can verify with `sha256sum -c` instead. If `~/.local/bin` is not on
your PATH, add it in your shell profile:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

The archive contains plain files at its root, so you can inspect it with
`tar -tzf "$ARCHIVE"` before extracting. No command is piped into a shell.

### Start on macOS

```sh
sessions install
open http://localhost:8787
```

### Start on Linux

`sessions install` currently supports macOS launchd only. On Linux, run the
daemon under your user supervisor or start it in the foreground:

```sh
SESSIONS_HOST=127.0.0.1 SESSIONS_PORT=8787 sessionsd
```

Then open `http://localhost:8787` and run `sessions token` in another terminal.
Linux systemd unit installation is not shipped yet.

## Listener and state

The default listener is loopback-only at `127.0.0.1:8787`. The daemon refuses
wildcard hosts such as `0.0.0.0`. `SESSIONS_HOST` and `SESSIONS_PORT` select a
specific alternative address and port.

Runtime state is under `~/.local/state/sessions/`, with runner artifacts in
`~/.local/state/sessions/runners/`. The lane ledger is stored beside them at
`~/.local/state/sessions/ledger/lanes.sqlite3`. On macOS only, an installation
that already wrote the earlier ledger under
`~/Library/Application Support/sessions/ledger/lanes.sqlite3` keeps using that
file instead of starting an empty one; the legacy path is adopted when present
but never created. Treat every one of these locations as private user data.

## Upgrade

Homebrew:

```sh
brew update
brew upgrade sessions
sessions install
```

Static install: download and verify the new archive, then replace all three
binaries together. Restart only `sessionsd`; per-session runner processes are
separate and continue to own their PTYs.

## Uninstall

There are two removal paths, because there are two things that install.

For the runtime you installed yourself, `sessions uninstall` stops the
development daemon and removes its launchd registration idempotently on macOS:

```sh
sessions uninstall
```

Then use `brew uninstall sessions`, or remove `sessions`, `sessionsd`, and `sessions-runner`
from the directory where you installed the static archive.

For Sessions.app, the app binary removes the integration points the package
wrote outside itself — the LaunchAgent that brings the daemon back at every
login, and the Sessions-managed `sessions` symlinks in
`/opt/homebrew/bin`, `/usr/local/bin`, and `~/.local/bin`:

```sh
/Applications/Sessions.app/Contents/MacOS/Sessions --remove-integration
```

Windows runs the same step automatically from the uninstaller, where it also
clears the logon supervisor value and the managed PATH entry. Run it by hand on
macOS, which ships as a bundle with no uninstaller of its own. It prints what it
removed, what was already absent, and what it kept, and exits non-zero naming
anything it could not finish.

You do not need to end sessions first. This path deliberately stops no process:
the daemon is the only thing that can still record what a live runner produces,
so ending it during removal would turn live sessions into orphans. Deleting the
LaunchAgent is enough — the daemon does not come back after the next login. A
`sessions` symlink that is a real file, or that points outside Sessions' managed
runtime, belongs to whoever put it there and is left exactly as found.

State is deliberately not deleted during either path. Session records, the
ledger, the saved port, and paired-machine credentials all survive. After
confirming no session or recovery data is needed, you may remove it separately.

## Troubleshooting

Run the built-in checks first:

```sh
sessions doctor
```

Common checks:

- **`sessions: command not found`:** confirm the install directory is on `PATH`.
- **Missing daemon or runner:** install all three binaries into the same
  directory and rerun `sessions install` on macOS.
- **Daemon unhealthy:** inspect
  `~/Library/Logs/sessions/tech.somewhere.sessions.dev.daemon.log` on macOS.
- **Web UI says unauthorized:** run `sessions token`, then paste the token into
  the UI's server settings.
- **Port already in use:** choose a private scratch port with `SESSIONS_PORT` or
  stop the other local process; do not expose a wildcard listener.
- **Lost lanes:** run `sessions recover`, review the plan, then opt in with
  `sessions recover --reopen`.

For remote setup, see [Network security](NETWORK_SECURITY.md). For signed
updates, rollback boundaries, and release recovery, see
[Release and distribution](RELEASE.md).
