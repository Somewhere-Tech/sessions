# Sessions 0.2.11

- Makes continuation chains explicit: an ended runtime now points to its newer
  live continuation, opens that continuation directly, and does not offer a
  duplicate resume.
- Clarifies that saved history belongs to a machine without implying the
  original runtime is still running there.
- Keeps the lifecycle and continuation card visible while reading long saved
  conversations and adds a floating jump-to-latest control.
- Tightens the global Find or run control so it remains readable in compact
  sidebars.
