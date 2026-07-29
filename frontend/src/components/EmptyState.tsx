interface Props {
  onNew: () => void;
}

export function EmptyState({ onNew }: Props): JSX.Element {
  return (
    <div className="empty-state">
      <div className="empty-workspace-mark" aria-hidden>
        <span /><span /><span />
      </div>
      <div className="empty-copy">
        <span className="empty-kicker">Agent operations inbox</span>
        <h1>Start your first session</h1>
        <p>Open Claude, Codex, or a shell in any workspace. Sessions keeps it running and brings it back when it needs you.</p>
        <button className="btn btn-primary empty-new-session" onClick={onNew}>＋ New Session</button>
        <div className="empty-capabilities" aria-label="Sessions capabilities">
          <span>Local by default</span><span>Survives app restarts</span><span>Claude · Codex · Shell</span>
        </div>
      </div>
    </div>
  );
}
