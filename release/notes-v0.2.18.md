# Sessions 0.2.18

- Lets a heavily used Sessions host finish a safe background-service update even when enumerating hundreds of retained sessions takes longer than a fast health probe.
- Keeps the short timeout for basic health checks while giving the session-preservation check enough time to prove every pre-update session remains present.
