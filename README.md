# Sessions

Sessions keeps Claude Code, Codex, shells, and other terminal programs alive
behind Sessions.app and a CLI. Each session has its own supervised runner,
hosting either a PTY or a structured provider conversation, so work survives a
daemon or UI restart and can be reopened from a native client.

## Trust contract

- **Local runtime:** Sessions drives tools you already installed through a PTY or
  their structured CLI contracts. It makes no direct model API calls.
- **Local by default:** the daemon listens on `127.0.0.1:8787`; remote access is
  opt-in and goes directly over your Tailscale network.
- **No default phone-home:** no Sessions account, analytics, telemetry, or relay.
  Opt-in backup, web push, and Tailscale use their configured services.
- **Native package:** Sessions.app is the primary macOS distribution and keeps
  its Go daemon and runner processes independent. The standalone runtime
  package exposes the same three `CGO_ENABLED=0` binaries directly.
- **Auditable:** source available under the [MIT license](LICENSE).

Sessions does not replace Claude Code or Codex. Install and authenticate the
agent CLI you want to run separately.

## Install

Sessions.app is the primary macOS package. The current signed, notarized Apple
Silicon build is available from
[GitHub Releases](https://github.com/somewhere-tech/sessions/releases/latest) or
Homebrew:

```sh
brew install --cask somewhere-tech/tap/sessions-app
```

For agents, headless machines, and Linux, install the three-binary runtime:

```sh
brew install somewhere-tech/tap/sessions
sessions install
```

Release automation also produces static archives for macOS arm64 and Linux
arm64/amd64. An agent can fetch an exact immutable version without parsing a
web page:

```sh
VERSION=0.1.0
ARCHIVE="sessions_${VERSION}_darwin_arm64.tar.gz"
gh release download "v${VERSION}" --repo somewhere-tech/sessions \
  --pattern "$ARCHIVE" --pattern "$ARCHIVE.sha256"
shasum -a 256 -c "$ARCHIVE.sha256"
tar -xzf "$ARCHIVE"
mkdir -p "$HOME/.local/bin"
install -m 0755 sessions sessionsd sessions-runner "$HOME/.local/bin/"
sessions install
open http://localhost:8787
```

`sessions install` registers `sessionsd` as the per-user development LaunchAgent
`tech.somewhere.sessions.dev.daemon`, starts it, and checks its health. Override the
label explicitly with `SESSIONS_DAEMON_LABEL` when needed. Direct loopback use is
zero-setup; LAN and remote clients normally authenticate with the token printed
by the command. Print it again later with `sessions token`.

Homebrew is the npm-like one-command runtime channel, but it installs native Go
binaries rather than a Node wrapper. There is no `curl | sh` installer. See
[installation details](docs/INSTALL.md) for exact archive names, agent-safe
downloads, PATH setup, Linux startup, upgrades, and uninstalling.

## Quickstart

```sh
id=$(sessions new --tool claude --cwd "$HOME/project" --name docs)
sessions send "$id" "Review the documentation and fix stale examples"
sessions wait "$id" --timeout 10m --summary
sessions last "$id" --role assistant
```

Open Sessions.app for terminal and structured conversation views; its History
surface browses the same conversations `sessions history` does, and typing
narrows them. Session IDs may be replaced with a unique prefix shown by
`sessions ls`.

Agents created from a managed parent run with autonomous full access by
default, so delegated work finishes in the background instead of waiting on a
person for each command; you can narrow this in Settings so children inherit
their parent's exact provider permission mode. A child can never widen its own
access past what the machine allows. An agent-created task worker also closes its runtime after a successful
final response while its transcript, lineage, and workspace remain available.
If a provider is waiting for approval, `sessions wait` returns `reason:
needs-input` with the actual prompt instead of pretending that the worker is
still making progress; `--summary` adds prose but never changes the shape.
Waiting on a session always answers with one JSON object and an exit code that
agrees with it: 0 satisfied, 1 usage, 2 daemon unreachable, 3 timed out, 4 the
target is gone or failed. The delegated-access choice is made during
onboarding and can be changed later in Settings.

## The CLI in 60 seconds

No skill or plugin is required. `sessions help` lists every command,
`sessions help <command>` gives focused usage and examples, and `sessions docs`
prints the complete offline Markdown reference directly from the executable's
command registry.

| Command | Purpose |
| --- | --- |
| `sessions new --tool claude\|codex\|shell [--cwd DIR]` | Start an interactive session |
| `sessions ls` | List the live sessions Sessions itself created |
| `sessions history [QUERY]` | Browse and preview every recorded Claude and Codex conversation, whoever started it |
| `sessions send <id> <message...>` | Submit text and confirm receipt |
| `sessions ask <id> <message...>` | Send, wait, and print the reply |
| `sessions wait <id> [--timeout 10m] [--summary]` | Wait for completion or return an actionable approval prompt |
| `sessions run [options] -- <command...>` | Start a tracked headless lane |
| `sessions lanes` | List running and completed lanes |
| `sessions status <id>` | Show compact session, git, activity, and verdict state |
| `sessions recover [--reopen]` | Inspect or reopen unexpectedly lost lanes |
| `sessions grep -C 3 "Google Ads"` | Search Claude and Codex history across every approved machine |
| `sessions cat <machine::history-id>` | Read the complete durable conversation returned by search |
| `sessions resume <[machine::]name-or-id> [--with claude\|codex]` | Reopen the exact conversation, or start a cross-provider continuation |
| `sessions fork <live-id> [--with claude\|codex]` | Branch a live chat without stopping the original |
| `sessions remote enable\|status\|disable` | Manage early-access Tailscale HTTPS access |
| `sessions model <id> <model> [--effort LEVEL]` | Switch an idle supported Claude session model |
| `sessions support [--diagnostics]` | Open feedback/support channels and preview a redacted local diagnostic summary |
| `sessions kill <id> [<id>...]` | Explicitly terminate selected sessions |

`continue` and `resurrect` are accepted spellings of `resume`. `ls` lists only
what Sessions itself started, so a conversation you opened by running plain
`claude` or `codex` is recorded but never appears there — `sessions history`
is the view that reaches it, with no search term required and the command that
reopens each row printed beside it.

Fleet search is the default when no connection flag is supplied. Use global
`--machine NAME` before a command to target only one approved machine. Search
and history report unreachable machines as partial coverage and never copy
transcripts just to make them greppable.

Also useful: `sessions snap`, `last`, `transcript`, `tail`, `keys`, `attach`,
`verdict`, `doctor`, `docs`, and `help`. Global flags are `--json`, `--host`, and
`--port`, and `--machine` (or `SESSIONS_HOST` / `SESSIONS_PORT`).

## Feedback and support

Run `sessions support` for the official feedback, public bug-ticket, and
private security-report links. `sessions support --diagnostics` adds a small
local preview with only versions, platform, daemon readiness, and a session
count. It uploads nothing and excludes session content, IDs, paths,
credentials, environment, logs, and crash files. Review and paste only what
you choose. Agents should use `sessions --json support --diagnostics`, add the
sanitized failing command shape/action, exit code, expected behavior, and exact error,
then ask the user before opening or submitting a ticket. Bug and feedback forms
record whether the report came from an agent, direct use, or both.

## Documentation

- [Agent entry point and repository rules](AGENTS.md)
- [Source-derived codebase guide](docs/CODEBASE.md)
- [Generated CLI reference](docs/CLI.md)
- [Conversation continuation and cross-provider behavior](docs/CONTINUATION.md)
- [Open-source and private-service boundary](docs/OPEN_SOURCE_BOUNDARY.md)
- [Product principles](docs/PRINCIPLES.md)
- [Lanes: delegated work, approvals, hand-back](docs/LANES.md)
- [Native app package and lifetime contract](docs/NATIVE_APP.md)
- [Fleet discovery, pairing, and trust without an account](docs/FLEET.md)
- [Android client, pairing, and development build](docs/ANDROID.md)
- [iOS client, pairing, and simulator build](docs/IOS.md)
- [Broad public roadmap](ROADMAP.md)

## Notifications and hooks

Enable browser push in **Settings → Notify when a session finishes**. Sessions
classifies a completed turn locally and sends done, blocked, or error notices to
the browser subscription you approved.

Run a per-session shell hook after a working-to-idle transition:

```sh
sessions new --tool codex --on-idle 'printf "%s: %s\n" "$SESSIONS_SESSION_NAME" "$SESSIONS_OUTCOME"'
```

A global `{"onIdle":"..."}` hook may be stored at
`~/.config/sessions/hooks.json`. Hooks receive `SESSIONS_SESSION_ID`,
`SESSIONS_SESSION_NAME`, `SESSIONS_SESSION_TOOL`, `SESSIONS_SESSION_CWD`,
`SESSIONS_FINAL_MESSAGE`, `SESSIONS_OUTCOME`, and `SESSIONS_DURATION_MS`.

## Remote access (early access)

Install Tailscale on the daemon host and your client device, then run:

```sh
sessions remote enable
sessions remote status
```

Sessions configures Tailscale Serve, verifies the HTTPS health endpoint, and
prints a QR code. Terminal data is not relayed by Sessions. Tailscale HTTPS issues
a certificate whose machine/tailnet name appears in public Certificate
Transparency logs. Run `sessions remote disable` to remove Sessions' Serve route.

Never bind the daemon to `0.0.0.0`; the binary refuses wildcard listeners.

## Troubleshooting

Start with:

```sh
sessions doctor
sessions status <id>
```

The native app's daemon log on macOS is
`~/Library/Logs/Sessions/sessionsd.log`. A standalone development daemon logs
to `~/Library/Logs/sessions/tech.somewhere.sessions.dev.daemon.log`. If the web UI cannot
authenticate, run `sessions token`. See
[installation troubleshooting](docs/INSTALL.md#troubleshooting).

## Development

The Go runtime is in `runtime/`; Sessions.app is in `src-tauri/`.

```sh
make -C runtime binaries
make -C runtime binaries-noui  # fast Go-only iteration
cd runtime && go test ./...
```

Tracked, non-interactive work uses lanes:

```sh
sessions run --name checks --cwd "$PWD" -- sh -lc 'cd runtime && go test ./...'
sessions lanes
```

See [architecture](ARCHITECTURE.md), [Go port constraints](runtime/ARCHITECTURE.md),
[Android development](docs/ANDROID.md), [iOS development](docs/IOS.md), and
[release instructions](docs/RELEASE.md).
