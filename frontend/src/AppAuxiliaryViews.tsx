import { useEffect, useMemo, useState } from 'react';
import { SessionView } from './components/SessionView';
import { ParserIcon } from './components/ParserIcon';
import { formatServerEndpoint } from './lib/serverEndpoint';
import { getActiveServer, useServers } from './lib/servers';
import { useTabLabel } from './lib/tabLabels';
import { handleExternalLinkClick } from './lib/externalLinks';
import type { TextSize } from './lib/textSize';
import { INITIAL_STATUS, type ActiveStatus } from './lib/activeStatus';
import type { SessionInfo } from './types';

export function DaemonBanner({
  error,
  onRetry
}: {
  error: string;
  onRetry: () => void;
}): JSX.Element {
  const isAuthError = /\b401\b/.test(error);
  const server = getActiveServer();
  const [tokenInput, setTokenInput] = useState('');
  const [tokenSaveError, setTokenSaveError] = useState('');

  const handleTokenSubmit = async (): Promise<void> => {
    const token = tokenInput.trim();
    if (!token) return;
    // Save the pasted token onto the active server config, then retry.
    setTokenSaveError('');
    try {
      await useServers.getState().updateServer(server.id, { token });
      onRetry();
    } catch (reason) {
      setTokenSaveError(
        reason instanceof Error
          ? reason.message
          : 'Sessions could not protect and save this machine credential.'
      );
    }
  };

  return (
    <div className="daemon-banner">
      {isAuthError ? (
        <>
          <p className="daemon-banner-title">Authentication required</p>
          <p className="daemon-banner-host">{formatServerEndpoint(server)}</p>
          <p className="daemon-banner-hint">Enter the daemon token to connect.</p>
          <div className="daemon-banner-token-row">
            <input
              type="password"
              className="daemon-banner-token-input"
              placeholder="Token"
              value={tokenInput}
              autoFocus
              onChange={(e) => setTokenInput(e.target.value)}
              onKeyDown={(e) => { if (e.key === 'Enter') void handleTokenSubmit(); }}
            />
            <button
              type="button"
              className="btn btn-primary daemon-banner-token-submit"
              disabled={!tokenInput.trim()}
              onClick={() => void handleTokenSubmit()}
            >
              Connect
            </button>
          </div>
          {tokenSaveError ? <p className="daemon-banner-hint" role="alert">{tokenSaveError}</p> : null}
        </>
      ) : (
        <>
          <p className="daemon-banner-title">Daemon unreachable</p>
          <p className="daemon-banner-host">{server.host}:{server.port}</p>
          <p className="daemon-banner-hint">
            sessionsd is not responding. Check that it is running on{' '}
            <strong>{server.host}</strong> port <strong>{server.port}</strong>.
          </p>
          <button
            type="button"
            className="btn daemon-banner-retry"
            onClick={onRetry}
          >
            Retry
          </button>
        </>
      )}
    </div>
  );
}

// Pop-out window shell. Renders a minimal top bar showing WHICH session
// this window is attached to (cwd basename + parser icon + working
// indicator) so the user can tell windows apart at a glance. Also
// drives document.title — Tauri's native window title is initially
// set in Rust via .title(...), but the WebView replaces it with the
// HTML <title> on load; setting document.title here keeps the macOS
// title bar in sync with the session.
export function SinglePopOut({
  sessionId,
  sessions,
  textSize
}: {
  sessionId: string;
  sessions: SessionInfo[];
  textSize: TextSize;
}): JSX.Element {
  const session = sessions.find((s) => s.id === sessionId);
  const overrideLabel = useTabLabel(sessionId);
  // Display label — same resolution as SessionTabs:
  //   user sessions override > claude custom > claude ai-title > cwd > cmd > short id.
  const label = useMemo(() => {
    if (overrideLabel) return overrideLabel;
    if (!session) return 'session';
    if (session.claudeCustomTitle) return session.claudeCustomTitle;
    if (session.claudeAiTitle) return session.claudeAiTitle;
    if (session.cwd) {
      const parts = session.cwd.split('/').filter(Boolean);
      const last = parts[parts.length - 1];
      if (last) return last;
    }
    return session.cmd || session.id.slice(0, 6);
  }, [overrideLabel, session]);

  const [status, setStatus] = useState<ActiveStatus>(INITIAL_STATUS);
  const cwdShort = useMemo(() => {
    const c = session?.cwd ?? '';
    if (!c) return '';
    // Shorten the OS home dir to ~ for compactness, without hardcoding a
    // username — match the standard macOS (/Users/<user>) and Linux
    // (/home/<user>) home layouts so it works for any operator.
    return c.replace(/^\/(Users|home)\/[^/]+/, '~');
  }, [session?.cwd]);

  // Keep the OS window title (and tab title) in sync with the session
  // and its live status. The working glyph in the title is a useful
  // peripheral signal when the window is in the background.
  useEffect(() => {
    const workingMark = status.isWorking ? '✻ ' : '';
    document.title = `${workingMark}${label} — Sessions`;
  }, [label, status.isWorking]);

  return (
    <div className={`app-shell single-mode text-size-${textSize.toLowerCase()}`} onClickCapture={handleExternalLinkClick}>
      <header className="single-mode-header">
        <ParserIcon icon={status.parserIcon} size={18} />
        <span className="single-mode-label">{label}</span>
        {cwdShort ? <span className="single-mode-cwd">{cwdShort}</span> : null}
        <span className="single-mode-spacer" />
        {status.isWorking ? (
          <span className="single-mode-working" aria-label="working">✻ working</span>
        ) : (
          <span className="single-mode-idle" aria-label="idle">○ idle</span>
        )}
      </header>
      <SessionView
        key={sessionId}
        sessionId={sessionId}
        isActive
        preferFullTerminal
        onStatusChange={setStatus}
        onOpenSession={(nextSessionId) => {
          const next = new URL(window.location.href);
          next.searchParams.set('session', nextSessionId);
          window.location.assign(next);
        }}
      />
    </div>
  );
}
