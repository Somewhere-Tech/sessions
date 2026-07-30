# Sessions 0.2.9

- Makes reopening old work the center of Search and Resume. Results are grouped
  by provider conversation, prefer the provider's title, show human and agent
  matches clearly, preserve the query while inspecting a transcript, and
  continue the selected conversation directly instead of dropping into a
  generic launcher.
- Simplifies the Sessions workspace around live work and recently ended work.
  Session actions use literal language, grouped managers stay readable, ended
  sessions explain how they stopped, and pop-out views, working-set controls,
  feedback, provider/machine filters, and cross-provider continuation remain
  available without crowding the main conversation.
- Refines the new-session launcher into a short Agent, Machine, Folder flow.
  Claude Rich remains the default, models use provider-backed choices, Terminal
  stays available for exact CLI behavior, and advanced account, permission,
  tag, and worktree controls are kept out of the primary path.
- Improves terminal rendering and scroll ownership so interactive provider
  screens reflow and repaint without ghosted rows or moving the surrounding app.
- Hardens the local daemon boundary: browser-origin checks happen before API
  dispatch, credential administration stays local, CORS explicitly covers
  uploads and transcript schema headers, known routes return accurate method
  errors, typed lifecycle errors replace HTTP string matching, and Bonjour
  startup no longer blocks LAN status.
- Adds a reproducible Android preview workflow that produces a checksumed,
  sideloadable client APK, and expands Windows candidate verification to cover
  the complete shared-client smoke suite. Windows and Android artifacts remain
  previews until their respective hardware tests pass.
- Preserves durable runners and structured histories across discovery, slow
  viewers, daemon shutdown, update staging, and ambiguous provider state.
