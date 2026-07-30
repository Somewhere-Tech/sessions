interface Props {
  compact?: boolean;
  label?: string;
}

export function LoadingShell({ compact = false, label = 'Loading sessions' }: Props): JSX.Element {
  return (
    <div className={`loading-shell${compact ? ' is-compact' : ''}`} role="status" aria-busy="true" aria-label={label}>
      <div className="loading-shell-header">
        <span className="loading-skeleton is-short" />
        <span className="loading-skeleton is-pill" />
      </div>
      <div className="loading-shell-body">
        <span className="loading-skeleton is-title" />
        <span className="loading-skeleton is-line" />
        <span className="loading-skeleton is-line is-medium" />
        <div className="loading-shell-card">
          <span className="loading-skeleton is-line is-short" />
          <span className="loading-skeleton is-line" />
          <span className="loading-skeleton is-line is-medium" />
        </div>
      </div>
      <span className="sr-only">{label}…</span>
    </div>
  );
}

export function SessionsWorkspaceSkeleton(): JSX.Element {
  return (
    <div className="workspace-loading-shell" role="status" aria-busy="true" aria-label="Loading your sessions">
      <div className="workspace-loading-navigator">
        <span className="loading-skeleton is-title" />
        {Array.from({ length: 6 }, (_, index) => (
          <div className="workspace-loading-row" key={index}>
            <span className="loading-skeleton is-dot" />
            <span className={`loading-skeleton is-line${index % 2 ? ' is-medium' : ''}`} />
          </div>
        ))}
      </div>
      <LoadingShell label="Loading the selected conversation" />
    </div>
  );
}
