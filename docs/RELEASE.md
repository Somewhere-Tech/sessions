# Release Sessions

Sessions.app is the primary macOS release vehicle. Standalone Go archives remain
a secondary headless/developer distribution, not the product's main install or
update story.

## Current state

The checked-in Tauri application builds as `Sessions.app`. It bundles signed Go
binaries and implements idempotent first install, health/discovery checks,
live-session baseline verification, rollback for daemon upgrades, and a signed
Tauri updater. Public releases use immutable GitHub tags, and the signed updater
manifest is live at
`https://sessions.somewhere.tech/releases/latest.json`. The archive is
Developer ID signed, notarized, stapled, and Gatekeeper accepted. Future
versions must preserve that artifact-first publication order.

Developer builds may use their own updater key:

```sh
npm run bootstrap
TAURI_SIGNING_PRIVATE_KEY=/path/to/development-updater.key TAURI_SIGNING_PRIVATE_KEY_PASSWORD='' npm run tauri:build
```

The resulting `.app` is a local development artifact. A Developer ID signature
alone is not a public release: the final bundle must also be notarized and
stapled.

## macOS app release gate

Before publishing a version:

1. Start from a clean reviewed commit on the current product branch.
2. Run the full Go, frontend, and Rust gates.
3. Build signed embedded Go binaries and the containing app.
4. Verify every nested executable with `codesign --verify --strict`.
5. Exercise a first install and an upgrade against scratch state.
6. Prove the pre/post session baseline and rollback behavior.
7. Submit for notarization, staple the ticket, and require `spctl` acceptance.
8. Sign and publish the updater manifest and immutable app artifact.
9. Install through the same channel a customer will use and repeat the health
   and session-adoption check.
10. From the previous installed app, run the newly built CLI's
    `sessions update --check`, then `sessions update`; confirm the pinned
    signature, Developer ID, Gatekeeper, atomic swap, app relaunch, managed CLI
    link, daemon version, and complete runner baseline.

The required Apple credential is an app-specific password or App Store Connect
API key. Credentials are release secrets and must never be committed.

For the Apple-account path, sign in at `https://account.apple.com`, open
**Sign-In and Security → App-Specific Passwords**, and generate one named for
Sessions notarization. Two-factor authentication must already be enabled. Use
that generated value as `APPLE_PASSWORD` for one release shell; never use the
primary Apple Account password and never write either value to this repository.

```sh
export APPLE_ID='your Apple Account email'
export APPLE_PASSWORD='the generated app-specific password'
export APPLE_TEAM_ID='7GW9T5ZWW8'
```

## Reproducible updater release

The production updater public key is committed at `release/updater.pub`; its
private half lives outside the repository at
`~/.config/sessions/sessions-updater.key` with mode `0600`. Back it up securely:
losing it prevents every installed build from accepting future updates.

Keep the version synchronized in `src-tauri/tauri.conf.json`,
`src-tauri/Cargo.toml`, and `frontend/package.json`, prepare release notes, then
run the non-mutating preflight:

```sh
scripts/release-app.sh --version 0.1.0 --notes-file /path/to/notes.md --dry-run
```

After exporting Apple notarization credentials, run the same command without
`--dry-run`. It builds and signs `Sessions.app`, verifies nested runtime
binaries, validates notarization/stapling/Gatekeeper, and writes
`release/out/v<version>/latest.json`. It does not publish or install anything.

## GitHub Actions release lane

`.github/workflows/ci.yml` runs the Go, generated-docs, frontend, and Rust gates
on every pull request and push to `main`. `.github/workflows/release.yml` accepts
only an existing `vX.Y.Z` tag contained in `main`. On GitHub's Apple Silicon
macOS runner it repeats the full gate, imports a dedicated Developer ID
certificate into a temporary keychain, notarizes and staples Sessions.app,
builds all standalone runtime archives, verifies their checksums, creates a
draft GitHub Release, uploads every asset, and makes the release visible only
after all uploads succeed.

Configure these secrets in the GitHub `release` environment:

- `APPLE_CERTIFICATE` — base64 of the exported Developer ID Application `.p12`.
- `APPLE_CERTIFICATE_PASSWORD` — the export password for that `.p12`.
- `SESSIONS_UPDATER_PRIVATE_KEY` — the contents of the production updater key.
- `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` — only when the updater key is encrypted.
- Either `APPLE_ID` plus the app-specific `APPLE_PASSWORD`, or
  `APPLE_API_ISSUER`, `APPLE_API_KEY`, and `APPLE_API_PRIVATE_KEY` for a
  dedicated App Store Connect key.

GitHub does not return secret values through its UI or API after storage;
authorized release workflows can consume them. This workflow stages them only
under the ephemeral runner's temporary directory and deletes those files in an
always-run cleanup step. Normal pull-request CI never receives release secrets
and has read-only repository permission.

Push a reviewed tag to start a release, or rerun an existing tag from the
workflow's manual dispatch:

```sh
git tag -a v0.1.0 -m 'Sessions 0.1.0'
git push origin v0.1.0
```

The workflow publishes GitHub's immutable artifacts, including an initial-install
zip, updater archive and signature, static runtime archives, individual
checksums, and `checksums.txt`. It deliberately does not receive broad Somewhere
project credentials.

The initial v0.1.0 release was built and notarized on the signing Mac, then
uploaded after all local gates and a GitHub download round-trip matched. The
tag-triggered lane remains the required path for subsequent releases once its
dedicated Apple certificate and notarization secrets are configured.

Upload `Sessions.app.tar.gz` and its `.sig` to the immutable GitHub release tag
first; the CI lane performs that upload for subsequent versions. Then use the Somewhere project's
`project_patch` operation to replace only `releases/latest.json` with the
rendered manifest from the workflow. Do not deploy a one-file directory: a full
static deploy can remove the onboarding pages. Read the file back from
production and install through the app's Settings → Sessions updates flow before
announcing the release.

For 0.2.2 and later, also exercise the terminal path:

```sh
sessions update --check
sessions update
```

The command deliberately has no alternate URL, key, artifact, destination, or
downgrade flag. It installs the whole native package, not only the currently
running CLI binary. The reopened app then stages the embedded runtime and
updates the managed CLI link. A temporary previous app exists only inside the
same-disk transaction and is removed after post-install verification.

## Standalone binary archives

The secondary archive builder remains available for automation and unsupported
headless installs:

```sh
./runtime/scripts/release.sh --version 0.1.0 --dry-run
./runtime/scripts/release.sh --version 0.1.0
```

Each archive contains adjacent `sessions`, `sessionsd`, and `sessions-runner` binaries,
`LICENSE`, and `README.md`, with a matching SHA-256 file. Homebrew may remain a
power-user channel, but its formula is not the native app updater and must not
be presented as the primary macOS experience.

## Existing installations

Exercise the public updater against both an idle host and a host with disposable
live sessions before publication. Record session IDs and runner PIDs before the
update and verify the exact baseline afterward. Do not stop or replace a live
runner to make its embedded version equal the newly installed daemon; the
compatibility contract intentionally allows an adopted runner to keep its
immutable runtime until it exits.
