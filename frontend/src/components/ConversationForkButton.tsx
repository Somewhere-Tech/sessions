interface Props {
  active: boolean;
  onClick: () => void;
}

export function ConversationForkButton({ active, onClick }: Props): JSX.Element {
  return (
    <button
      type="button"
      className={`conversation-fork-toggle${active ? ' is-active' : ''}`}
      aria-pressed={active}
      title={active ? 'Stop choosing a fork point' : 'Choose a message to fork from'}
      onClick={onClick}
    >
      <ForkIcon />
      <span>{active ? 'Cancel fork' : 'Fork'}</span>
    </button>
  );
}

export function ForkIcon(): JSX.Element {
  return (
    <svg viewBox="0 0 18 18" aria-hidden="true">
      <circle cx="5" cy="4" r="1.75" />
      <circle cx="13" cy="4" r="1.75" />
      <circle cx="13" cy="14" r="1.75" />
      <path d="M5 5.75v2.1A3.15 3.15 0 0 0 8.15 11H10a3 3 0 0 1 3 3" />
      <path d="M5 7.85A3.15 3.15 0 0 1 8.15 4H11.2" />
    </svg>
  );
}
