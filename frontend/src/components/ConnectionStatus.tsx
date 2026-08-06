interface Props {
  machine: string;
  hydrated: boolean;
  error: string | null;
}

// This indicator deliberately describes only the selected machine. Session
// stream state belongs inside the session toolbar; ended history has no
// connection state at all.
export function ConnectionStatus({ machine, hydrated, error }: Props): JSX.Element | null {
  if (hydrated && !error) return null;

  const unreachable = Boolean(error);
  const label = unreachable
    ? `Can’t reach ${machine}`
    : `Reconnecting to ${machine}… Agents keep running`;

  return (
    <span
      className={`conn-status ${unreachable ? 'conn-disconnected' : 'conn-reconnecting'}`}
      role="status"
      aria-live="polite"
      title={error || label}
    >
      <span className="conn-dot" aria-hidden />
      <span className="conn-label">{label}</span>
    </span>
  );
}
