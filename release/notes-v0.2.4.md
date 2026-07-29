# Sessions 0.2.4

- Adds native Bonjour discovery for Sessions hosts on a trusted private
  network. Discovery reveals no credentials or session metadata and still
  requires an explicit request, host approval, and revocable per-device token.
- Adds agent-native machine management through `sessions machines
  discover|connect|list|forget`, `sessions access requests|accept|deny`, and
  `sessions --machine NAME ...`.
- Keeps Tailscale and nearby-network discovery independent so either transport
  can remain useful when the other is unavailable.
- Hardens remote access by preventing a raw non-loopback `--host` from reusing
  the local daemon master token. Nearby LAN traffic is labeled as unencrypted
  and for trusted private networks only.
- Includes the Daily journal, interface scaling and contrast improvements, and
  the clearer Sessions product page added since 0.2.3.
- Preserves the runner-safe updater contract: only the app and daemon advance;
  active runners continue from their immutable runtime and are re-adopted by
  exact session ID.
