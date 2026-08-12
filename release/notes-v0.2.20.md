# Sessions 0.2.20

- Lets the safe macOS background-service updater wait through a long retained-session discovery sweep before capturing its preservation baseline.
- Prevents heavily used hosts from staging a new release and then silently leaving the previous daemon active because the local session inventory needed more than 30 seconds to respond.
- Keeps the existing bounded 15-minute ceiling and never replaces the daemon unless the pre-update live-session baseline can be read successfully.
