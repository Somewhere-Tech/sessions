# Sessions 0.2.23

- Restores accelerated macOS terminal rendering so provider pickers and other
  full-screen terminal interfaces respond immediately while retaining a narrow
  repair for explicit screen resets.
- Bounds the structured-event window held in runner and daemon memory while
  preserving complete append-only provider history on disk.
- Lets a newly updated daemon adopt older durable runners without retaining an
  unbounded replay or restarting the work they own.
- Avoids repeated socket retries for runner artifacts whose recorded process is
  definitely gone, keeping restart recovery responsive on long-lived hosts.
