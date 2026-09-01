# Sessions 0.2.26

- Keeps signed app updates bounded to live runner processes instead of treating retained history as work that must be re-adopted.
- Prevents heavily used Macs from rolling a healthy background-service update back merely because old, non-running session records changed during restart.
- Preserves compatibility with older daemons that did not yet report runner process identifiers.

Updating the app does not stop running sessions. Existing runners continue on their compatible bundled version until they finish.
