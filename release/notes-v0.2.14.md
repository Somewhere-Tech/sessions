# Sessions 0.2.14

- Prevents Rich Claude sessions from showing an impossible Terminal prompt or
  offering a Terminal view that does not exist.
- Keeps Claude slash commands out of the conversation transcript instead of
  sending them as ordinary user messages.
- Makes `/rename` update the durable Sessions conversation name directly from
  a Rich session.
- Gives `/rc` an explicit path to end an idle Rich runtime and continue the
  same Claude conversation in Terminal with Remote Control enabled.
- Keeps drafts local while Claude is working; Sessions still does not create a
  hidden prompt queue.
- Adds matching CLI and API support for continuing a Claude conversation in
  Terminal with Remote Control.
