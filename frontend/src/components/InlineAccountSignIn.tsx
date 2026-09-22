import type { AccountProfile } from '../api/sessionsd';
import { SigningInCard, useGuidedAccountLogin } from './AccountSignIn';

export function InlineAccountSignIn({ tool, serverId, account, onReload, onConnected }: {
  tool: 'claude' | 'codex'; serverId: string; account?: AccountProfile;
  onReload: (profiles: AccountProfile[]) => void;
  onConnected: (profile: string) => void;
}): JSX.Element {
  const login = useGuidedAccountLogin({ serverId, onReload });
  return <div className="account-profile-field">
    {login.signingInFor ? <SigningInCard
      account={login.signingInFor} operation={login.operation} busy={login.busy}
      onCode={login.submitCode} onCancel={login.cancel}
      onDone={() => {
        if (login.operation?.state === 'connected') onConnected(login.signingInFor!.name);
        login.finishSignIn();
      }}
    /> : <button type="button" className="btn btn-primary" disabled={login.busy}
      onClick={() => { if (account) void login.startLogin(account); else void login.addAccount(tool, '', ''); }}>
      {login.busy ? 'Preparing sign-in…' : `Sign in to ${tool === 'claude' ? 'Claude' : 'ChatGPT'}`}
    </button>}
    {login.message ? <p role="status">{login.message}</p> : null}
  </div>;
}
