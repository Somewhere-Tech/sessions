export type Provider = 'claude' | 'codex';

export function normalizeProvider(value: string | undefined): Provider | null {
  if (value === 'claude' || value === 'claude-code') return 'claude';
  if (value === 'codex') return 'codex';
  return null;
}

export function ProviderBadge({ provider, compact = false, size }: { provider: Provider; compact?: boolean; size?: number }): JSX.Element {
  const label = provider === 'claude' ? 'Claude' : 'Codex';
  return (
    <span className={`provider-badge is-${provider}${compact ? ' is-compact' : ''}`}>
      <ProviderMark provider={provider} size={size ?? (compact ? 16 : 19)} />
      <span>{label}</span>
    </span>
  );
}

export function ProviderMark({ provider, size = 32 }: { provider: Provider; size?: number }): JSX.Element {
  return (
    <span
      className={`provider-mark is-${provider}`}
      style={{ width: size, height: size }}
      aria-hidden
    >
      {provider === 'claude' ? (
        <img src={`${import.meta.env.BASE_URL}claude.png`} alt="" draggable={false} />
      ) : (
        <img className="provider-mark-openai" src={`${import.meta.env.BASE_URL}openai-icon.svg`} alt="" draggable={false} />
      )}
    </span>
  );
}
