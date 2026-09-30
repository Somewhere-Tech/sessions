import { useEffect, useRef, useState } from 'react';
import { useServers } from '../lib/servers';
import { formatServerEndpoint } from '../lib/serverEndpoint';
import { rememberServerEndpoint } from '../lib/hostedBootstrap';
import { useSessions } from '../store/sessions';

// Compact dropdown that lives in the app header. Lists known sessionsd
// servers, marks the active one, and exposes "+ Add server" to register
// a new one (saved on this device; Windows tokens use the native vault).
// Switching the active server triggers
// re-fetch in useSessions, useTerminal, usePrettyParser, ReflowedView via
// each of their effect-key dependencies on activeServerId.
export function ServerSelector(): JSX.Element {
  const servers = useServers((s) => s.servers);
  const activeId = useServers((s) => s.activeId);
  const setActive = useServers((s) => s.setActive);
  const removeServer = useServers((s) => s.removeServer);
  // Surface refresh errors here — if the chosen server is unreachable
  // the user sees an unreachable badge on the trigger instead of just
  // an empty tab strip. error string clears on the next successful
  // refresh in the store.
  const sessionsError = useSessions((s) => s.error);
  const sessionsHydrated = useSessions((s) => s.hydrated);
  const unreachable = !!sessionsError && !sessionsHydrated;
  const active = servers.find((s) => s.id === activeId) ?? servers[0]!;

  const [open, setOpen] = useState(false);
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState('');
  const [endpoint, setEndpoint] = useState('');
  const [advanced, setAdvanced] = useState(false);
  const [token, setToken] = useState('');
  const [endpointError, setEndpointError] = useState('');
  const wrapRef = useRef<HTMLDivElement | null>(null);

  // Close the dropdown on outside click. Pointerdown so it closes BEFORE
  // a click on a tab below it triggers tab activation.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent): void => {
      if (!wrapRef.current) return;
      if (!wrapRef.current.contains(e.target as Node)) {
        setOpen(false);
        setAdding(false);
      }
    };
    document.addEventListener('pointerdown', onDown);
    return () => document.removeEventListener('pointerdown', onDown);
  }, [open]);

  const handleAdd = async (): Promise<void> => {
    try {
      await rememberServerEndpoint(endpoint, {
        name,
        token
      });
    } catch (error) {
      setEndpointError(error instanceof Error ? error.message : 'Enter a valid endpoint.');
      return;
    }
    setAdding(false);
    setOpen(false);
    setName('');
    setEndpoint('');
    setAdvanced(false);
    setToken('');
    setEndpointError('');
  };

  return (
    <div className="server-selector" ref={wrapRef}>
      <button
        type="button"
        className={`server-selector-trigger${unreachable ? ' is-unreachable' : ''}`}
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="listbox"
        aria-expanded={open}
        // Title carries the endpoint so it is available on hover without
        // taking up always-visible chrome. Rows show it in full below.
        title={
          unreachable
            ? `Couldn't reach ${formatServerEndpoint(active)}`
            : `${active.name} — ${formatServerEndpoint(active)}`
        }
      >
        <span className="server-selector-icon" aria-hidden>🖥</span>
        <span className="server-selector-name">{active.name}</span>
        {unreachable ? <span className="server-selector-warn" aria-hidden>⚠</span> : null}
        <span className="server-selector-caret" aria-hidden>▾</span>
      </button>
      {open ? (
        <div className="server-selector-menu" role="listbox">
          {servers.map((s) => (
            <div key={s.id} className={`server-selector-row${s.id === activeId ? ' is-active' : ''}`}>
              <button
                type="button"
                className="server-selector-pick"
                title={`${s.name} — ${formatServerEndpoint(s)}`}
                onClick={() => { setActive(s.id); setOpen(false); }}
              >
                <span className="server-selector-dot" aria-hidden>{s.id === activeId ? '●' : '○'}</span>
                <span className="server-selector-name">{s.name}</span>
                <span className="server-selector-host">{formatServerEndpoint(s)}</span>
              </button>
              {!s.isDefault && !s.relayMachineId ? (
                <button
                  type="button"
                  className="server-selector-remove"
                  title="Remove server"
                  onClick={() => {
                    void removeServer(s.id).catch((error) => {
                      setEndpointError(error instanceof Error ? error.message : 'Sessions could not forget this server.');
                    });
                  }}
                >
                  ×
                </button>
              ) : null}
            </div>
          ))}
          <div className="server-selector-divider" />
          {adding ? (
            <div className="server-selector-add-form">
              <input
                type="text"
                placeholder="Name (e.g. Mac Mini)"
                value={name}
                onChange={(e) => setName(e.target.value)}
                autoFocus
              />
              <input
                type="text"
                inputMode="url"
                autoComplete="url"
                placeholder="Endpoint (https://mac.example.com)"
                aria-label="Endpoint"
                aria-invalid={endpointError ? true : undefined}
                aria-describedby={endpointError ? 'server-endpoint-error' : undefined}
                value={endpoint}
                onChange={(e) => {
                  setEndpoint(e.target.value);
                  if (endpointError) setEndpointError('');
                }}
                onKeyDown={(e) => { if (e.key === 'Enter') void handleAdd(); }}
              />
              {endpointError ? (
                <p id="server-endpoint-error" className="server-selector-error" role="alert">
                  {endpointError}
                </p>
              ) : null}
              <button
                type="button"
                className="server-selector-advanced"
                aria-expanded={advanced}
                onClick={() => setAdvanced((value) => !value)}
              >
                <span aria-hidden>{advanced ? '▾' : '▸'}</span> Advanced
              </button>
              {advanced ? (
                <input
                  type="password"
                  autoComplete="off"
                  placeholder="Token (optional)"
                  aria-label="Token"
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  onKeyDown={(e) => { if (e.key === 'Enter') void handleAdd(); }}
                />
              ) : null}
              <div className="server-selector-add-actions">
                <button type="button" onClick={() => setAdding(false)}>Cancel</button>
                <button type="button" className="primary" onClick={() => void handleAdd()}>Add</button>
              </div>
            </div>
          ) : (
            <button
              type="button"
              className="server-selector-add"
              onClick={() => setAdding(true)}
            >
              + Add server…
            </button>
          )}
        </div>
      ) : null}
    </div>
  );
}
