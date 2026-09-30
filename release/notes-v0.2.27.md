# Sessions 0.2.27

## Distribution and verification

Source changes, development artifacts, signed drafts, published GitHub releases,
npm packages, and installed apps are distinct delivery states. These notes
describe the version's changes; they do not establish publication on every
channel or acceptance on every platform. Public promotion follows verification
of the exact signed delivery artifacts.

## Changes

- More trustworthy message delivery: durable receipts distinguish accepted,
  refused, and unknown outcomes, retain late acknowledgments, and make safe
  recovery explicit instead of encouraging duplicate sends.
- More accurate working, waiting-for-approval, resumed, and lost-session states.
  Live reads that cannot reach a paused runner return an actionable error
  instead of an empty successful result.
- Conversation continuation and restart controls preserve provider identity and
  expose access choices. Structured Codex continuations restore bounded display
  history; changing a viewer or daemon does not own a runner's lifetime.
- Project-first navigation, clearer account selection and usage, delegated-team
  views, and direct approval controls keep routine states calm and actionable.
- Fleet reads return partial coverage when a computer cannot be reached, use
  bounded per-machine work, and report the observed connection failure rather
  than guessing that macOS denied Local Network permission.
- Optional account claims retain replay protection across daemon process
  restart and account logout, through the full accepted timestamp window.
  Optional relay authorization supports explicit machine-key allow-lists;
  unsupported directory-token configuration returns setup instructions.
  Relay connectors keep each parsed request outside loopback authority and
  allow opaque duplex traffic only after a confirmed WebSocket upgrade.
- Incremental search indexing, retained history-listing caches, lazy secondary
  views, and bounded terminal buffers reduce repeated work on long histories.
  Codex metadata readers also avoid allocating the full 64 KiB bound upfront;
  the read limit and accepted metadata remain unchanged.
- Filesystem listings are complete up to 10,000 entries and return an explicit
  error beyond that limit. Each multiplexed connection admits at most 256
  attached or pending session streams; reaching the limit does not end work.
- Linux `sessions install` stages immutable runtime copies and registers a
  systemd user service. Restarting the daemon preserves independent runners;
  reboot recovery retains conversations for explicit resumption.
- The npm package wraps the same three native binaries, with pinned archive and
  executable checksums. Registry publication is separate from building or
  releasing those archives.
- Windows uses user-scope DPAPI for saved pairing tokens; macOS and iOS use
  Keychain, and Android uses Keystore. Migration from legacy client storage
  verifies native persistence first. This does not claim protected storage for
  Linux clients or browsers, or change provider-owned login credentials.
- Shared phone layouts use safe areas and readable input sizing. Phone clients
  present connected-host settings as host-owned rather than offering controls
  they cannot apply locally.
- Release automation separates source builds, signing authority, packaging,
  and draft staging. Publishing the unchanged signed draft is explicit after
  exact-byte native install/update acceptance; npm and updater promotion stay
  separate. Optional Apple Silicon development artifacts include a
  verified provenance receipt and are not stable updater releases.
  Initial-install ZIPs must also pass signature, stapled-ticket, Gatekeeper,
  and runtime-hash checks after extraction. Update convergence distinguishes
  authoritative session endings from missing or unreachable runners.

## Verification boundaries

Automated source, contract, fixture, and package tests do not prove every
provider or native-device journey. A development app is not notarized merely
because its CI build succeeded. A Linux archive is not a native lifecycle or
reboot test, and a frontend phone test is not physical-device acceptance.

Before publication, the exact macOS release bytes require nested Developer ID
signatures, notarization, stapling, Gatekeeper acceptance, and updater identity
verification. Native install/update checks must establish that compatible live
runners survive and that an unreachable baseline triggers the documented
recovery or rollback behavior. Windows and physical iOS/Android acceptance
remain separate from cross-compilation and simulated tests.

Existing sessions must not be ended merely to install a compatible update.
These notes do not assert that any user's machines have already been updated.
