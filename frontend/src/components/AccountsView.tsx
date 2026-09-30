import { useEffect, useState } from 'react';
import { fetchProfiles, type AccountProfile } from '../api/sessionsd';
import { AccountsPanel } from './AccountsPanel';

// Accounts is its own destination: the Claude and ChatGPT subscriptions each
// computer can run chats on. It reads the selected computer's list itself so
// opening it does not depend on Settings having loaded first.

export function AccountsView({ hostName, serverId }: { hostName: string; serverId?: string }): JSX.Element {
  const [profiles, setProfiles] = useState<AccountProfile[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    setProfiles(null);
    setError(null);
    void fetchProfiles(controller.signal, serverId)
      .then((list) => { if (!controller.signal.aborted) setProfiles(list); })
      .catch((reason) => {
        if (controller.signal.aborted) return;
        setError(reason instanceof Error ? reason.message : `${hostName} did not answer.`);
        setProfiles([]);
      });
    return () => controller.abort();
  }, [serverId, hostName]);

  return (
    <div className="accounts-view">
      {profiles === null ? (
        <section className="settings-page accounts-panel"><p role="status">Reading the accounts on {hostName}…</p></section>
      ) : (
        <>
          {error ? <p className="settings-page settings-message" role="status">Could not read the accounts on {hostName}: {error}</p> : null}
          <AccountsPanel profiles={profiles} machineName={hostName} serverId={serverId} onReload={setProfiles} />
        </>
      )}
    </div>
  );
}
