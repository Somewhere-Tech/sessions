# Sessions 0.2.12

- Makes Claude Rich the consistent default for new and continued
  conversations while keeping Terminal as an explicit compatibility choice.
- Adds safe Rich or Terminal selection when continuing the same provider
  conversation locally or on another Sessions machine. Source history and
  workspaces remain untouched.
- Adds durable session names across the app, Fleet, search, later
  continuations, and the new `sessions rename` CLI command.
- Imports Claude `/rename` titles when a Sessions title has not been chosen,
  without modifying unsupported provider-private history files.
- Removes the remaining legacy product wording from current public
  documentation while retaining narrowly scoped compatibility readers needed
  for older resumable runtimes.
