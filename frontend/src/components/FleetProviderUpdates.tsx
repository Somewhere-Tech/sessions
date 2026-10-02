import { useRef, useState } from 'react';
import { updateProvider, type ProviderStatus } from '../api/sessionsd/operations';
import { serverDisplayName, useServers, type ServerConfig } from '../lib/servers';

interface UpdateResult { id: string; name: string; status: 'waiting' | 'updating' | 'updated' | 'unknown' | 'skipped'; detail: string }

/** Per-machine results, never an all-or-nothing fleet success claim. */
export function FleetProviderUpdates(): JSX.Element {
  const servers = useServers((state) => state.servers);
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  const [results, setResults] = useState<UpdateResult[]>([]);
  const update = async (provider: ProviderStatus['id']): Promise<void> => {
    if (locked.current) return;
    locked.current = true; setBusy(true);
    const targets = [...servers];
    setResults(targets.map((server) => ({ id: server.id, name: serverDisplayName(server, true), status: 'waiting', detail: '' })));
    const publish = (server: ServerConfig, status: UpdateResult['status'], detail: string): void => {
      setResults((previous) => previous.map((result) => result.id === server.id ? { ...result, status, detail } : result));
    };
    const queue = [...targets];
    const worker = async (): Promise<void> => {
      for (let server = queue.shift(); server; server = queue.shift()) {
        if (server.directoryOnly) { publish(server, 'skipped', 'Pair this computer first.'); continue; }
        publish(server, 'updating', 'Using the provider’s official updater…');
        try {
          const response = await updateProvider(provider, server.id);
          publish(server, 'updated', response.provider.version || 'Update confirmed');
        } catch (error) {
          // A transport failure may follow a successful install. Do not call
          // that "not updated" or automatically invoke another installer.
          publish(server, 'unknown', error instanceof Error ? error.message : 'Check this computer before trying again.');
        }
      }
    };
    try { await Promise.all([worker(), worker()]); }
    finally { locked.current = false; setBusy(false); }
  };
  if (servers.length < 2) return <></>;
  return <div className="settings-card">
    <h2>Update across your computers</h2>
    <p>Updates the provider tool on each paired computer. Running agents keep their current process; new chats use the new version. Offline computers report separately.</p>
    <div className="dialog-actions">
      <button type="button" className="btn btn-secondary" disabled={busy} onClick={() => void update('claude')}>Update Claude everywhere</button>
      <button type="button" className="btn btn-secondary" disabled={busy} onClick={() => void update('codex')}>Update Codex everywhere</button>
    </div>
    <div role="status" aria-live="polite">{results.map((result) => <div className="settings-static-row" key={result.id}>
      <span><strong>{result.name}</strong><small>{result.detail}</small></span>
      <span>{result.status === 'unknown' ? 'Needs checking' : result.status}</span>
    </div>)}</div>
  </div>;
}
