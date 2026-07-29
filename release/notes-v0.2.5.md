# Sessions 0.2.5

- Fixes Mac-to-Mac nearby discovery by registering the Sessions proxy record
  through Apple's system Bonjour responder instead of competing with
  `mDNSResponder` for multicast traffic.
- Continues to advertise only the selected private LAN listener address and
  low-sensitivity protocol markers. Discovery remains a hint: clients verify
  Sessions health, then require explicit host approval and a revocable device
  credential.
- Normalizes DNS presentation escapes so nearby machine names render as human
  names rather than backslash-escaped service labels.
- Includes an opt-in two-machine acceptance test for native Bonjour
  registration. The release check discovered and verified a live Sessions
  endpoint before publication.
- Preserves the runner-safe updater contract: only the app and daemon advance;
  active runners continue from their immutable runtime and are re-adopted by
  exact session ID.
