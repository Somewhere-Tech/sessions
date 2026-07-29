# Install Sessions

Sessions.app is the primary macOS package. Its bundled-runtime installer and
signed updater shipped publicly as v0.1.0. The release is Developer ID signed,
notarized, stapled, and backed by an immutable updater artifact. The standalone
instructions below remain useful for agents, developers, and headless installs.
Do not use them to change the production mini.

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

## Homebrew runtime install on macOS

Homebrew is the npm-like package-manager channel for the three standalone
runtime binaries:

```sh
brew install somewhere-tech/tap/sessions
sessions install
open http://localhost:8787
```

The public `somewhere-tech/homebrew-tap` repository pins the immutable v0.1.0 URLs and
SHA-256 digests. It installs native binaries directly; Node and npm are not
runtime dependencies.

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
VERSION=0.1.0
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
VERSION=0.1.0
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
`~/.local/state/sessions/runners/`. The lane ledger is stored separately at
`~/Library/Application Support/sessions/ledger/lanes.sqlite3` in the current
implementation. Treat both locations as private user data.

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

End or record any sessions you still need before removing their runtime. On
macOS, stop the daemon and remove its launchd registration idempotently:

```sh
sessions uninstall
```

Then use `brew uninstall sessions`, or remove `sessions`, `sessionsd`, and `sessions-runner`
from the directory where you installed the static archive.

State is deliberately not deleted during uninstall. After confirming no
session or recovery data is needed, you may remove it separately.

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

For remote setup and operational recovery, see the [runbooks](RUNBOOKS.md).
