import type { SessionInfo } from '../types';

// A session on a second subscription looks exactly like one on the first until
// someone says so. This is that badge: it appears only when a session is not on
// the default account, and it says the account's name, never anything read out
// of a credential.
export function AccountBadge(
  { session, label, className }: { session: SessionInfo; label?: string; className?: string }
): JSX.Element | null {
  const account = session.profile?.trim();
  if (!account) return null;
  const name = label || (/^acct-[a-f0-9]+$/i.test(account) ? `${session.tool === 'codex' ? 'ChatGPT' : 'Claude'} account` : account);
  return (
    <span
      className={`account-badge${className ? ` ${className}` : ''}`}
      title={`This session uses the ${label || account} account`}
    >
      {name}
    </span>
  );
}
