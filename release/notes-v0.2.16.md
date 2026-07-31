# Sessions 0.2.16

- Makes Claude’s native interactive session the default and offers opt-in Remote Control during onboarding and in Settings, using the user’s Claude subscription directly.
- Adds non-destructive conversation forks from any durable user or agent message, including an explicit option to open the copy in the other provider.
- Keeps cross-machine continuation portable and reviewable: provider history and workspace references move, while credentials, attachments, and the source history stay put.
- Makes `sessions update` wait for the current daemon, CLI, and every pre-update live runner before reporting success.
