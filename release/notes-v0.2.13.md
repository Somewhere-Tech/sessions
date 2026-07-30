# Sessions 0.2.13

- Makes durable Sessions conversation names authoritative in every native
  view, including names changed through `sessions rename` or another machine.
- Keeps older browser-local and project-folder labels only as fallbacks for
  unnamed legacy conversations.
- Prevents renaming one conversation from silently applying that title to
  future sessions created in the same folder.
- Includes the 0.2.12 portable continuation work: Claude Rich is the default,
  Terminal remains an explicit compatibility choice, and a conversation can
  continue on another Sessions machine without changing its source history.
