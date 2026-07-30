# Sessions 0.2.10

- Includes the complete Sessions 0.2.9 workspace, search/resume, terminal,
  Fleet, launcher, local-security, Windows-preview, and Android-preview work.
- Corrects `sessions doctor` for durable native runners. Doctor now inspects
  the runner process itself instead of its intentionally replaceable parent,
  so healthy runners preserved across an app or daemon update are no longer
  told to recreate.
