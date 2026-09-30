# Open-source boundary

Sessions uses an open-core boundary centered on the software that runs on a
user's device.

## Open in this repository

- the local daemon, runner, and CLI;
- the desktop and mobile client source that speaks to that runtime;
- local storage formats and the versioned client/runner contracts;
- LAN and user-owned tailnet connectivity;
- the optional Somewhere sign-in and owner-scoped machine-directory service
  under `cloud/sessions-fleet`;
- build, test, contributor, security, and privacy documentation for shipped
  behavior.

A clean checkout of this repository must be sufficient to build and test the
local runtime and clients. Local operation must not require a Somewhere
account or a private package.

## Hosted service boundary

The minimal Somewhere account and machine-registration service is open here so
its stored metadata, request signatures, replay rules, and rate limits are
reviewable with the clients that use it. Other hosted integrations may be
developed separately, including managed backup/search services, relays,
billing, abuse controls, and specialized hosted workers. Those services consume
documented Sessions contracts; they do not replace the open local runtime or
silently change its trust model.

The product must label hosted behavior and its network effects explicitly.
The local default remains no account, no relay, and no Sessions telemetry.

## Documentation boundary

This repository documents shipped behavior, public architecture and protocol
contracts, contributor workflows, security/privacy properties, and broad
roadmap themes. Internal launch planning, unreleased implementation status,
commercial planning, private service design, dogfood notes, and rejected
product options belong in private project systems rather than the public
source tree.

Removing a file from the current tree does not remove it from Git history.
Maintainers audit both the current tree and history, rewrite public history
before the repository boundary is established, and rotate any exposed
credential. `scripts/check-public-tree.sh` prevents the reserved private paths
from being tracked again.

Publication is allowlisted. `scripts/public-paths.txt` is the complete set of
top-level paths eligible for a public checkout, `scripts/check-public-tree.sh`
rejects anything outside it (including reserved private service roots), and
`scripts/export-public-tree.sh` exports one committed revision using that
manifest. Private service source must live in a separate private repository.
The explicitly public `cloud/sessions-fleet` project is not a private-service
staging path; adding another hosted service requires separately reviewing this
allowlist and boundary.

## Contributions and releases

The public repository is canonical for open components so issues, pull
requests, source tags, and release artifacts share one review trail. Private
services pin an explicit public protocol/version. If a downstream distribution
is ever generated from a private monorepo, publication must be automated,
allowlisted, reproducible, and CI-checked rather than copied by hand.
