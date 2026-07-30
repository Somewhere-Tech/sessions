import { useEffect, useMemo, useRef, useState } from 'react';
import { useSessions } from '../store/sessions';
import {
  adoptConversation,
  fetchResumableSessions,
  repairAdoption,
  type AdoptConversationResult,
  type ResumableSession
} from '../api/sessionsd';
import { getCwdLabel } from '../lib/tabLabels';
import { providerConversationId } from '../lib/sessionStatus';
import { ProviderBadge } from './ProviderBadge';

// Dedicated resume picker — opened from an ended session or New Session
// (separate from "+ New session"). The old design tucked resume inside
// a tab on NewSessionDialog, which made it hard to scan: cramped rows,
// 100-char truncation, competing path/size/timestamps. This dialog is
// purpose-built for "which conversation am I picking up?" — message
// is the headline, everything else is quiet metadata.

interface Props {
  onClose: () => void;
  onResumed: (laneId: string) => void;
  preferredProviderId?: string;
  preferredSourceSessionId?: string;
  preferredHistoryId?: string;
  preferredDestinationProvider?: 'claude' | 'codex';
  // Click handler if the user wants to abandon resume and start fresh.
  // App.tsx swaps to the New Session dialog so the user doesn't lose
  // their place if the picker turns out to be empty.
  onStartNew: () => void;
}

type ViewMode = 'flat' | 'grouped';
type ProviderFilter = 'all' | 'claude' | 'codex';

const VIEW_MODE_KEY = 'sessions:resume-view-mode:v1';

function readViewMode(): ViewMode {
  try {
    const v = window.localStorage.getItem(VIEW_MODE_KEY);
    if (v === 'grouped' || v === 'flat') return v;
  } catch { /* ignore */ }
  return 'flat';
}

function writeViewMode(mode: ViewMode): void {
  try { window.localStorage.setItem(VIEW_MODE_KEY, mode); }
  catch { /* ignore */ }
}

function relativeWhen(ms: number): string {
  const diff = Date.now() - ms;
  if (diff < 60_000) return 'just now';
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`;
  if (diff < 604_800_000) return `${Math.floor(diff / 86_400_000)}d ago`;
  return new Date(ms).toLocaleDateString();
}

function shortFolder(cwd: string): string {
  const friendly = getCwdLabel(cwd);
  if (friendly) return friendly;
  const parts = cwd.split('/').filter(Boolean);
  return parts[parts.length - 1] ?? cwd;
}

function displayPath(cwd: string): string {
  // Shorten the OS home dir to ~ without hardcoding a username (macOS
  // /Users/<user>, Linux /home/<user>) — matches App.tsx's cwdShort.
  return cwd.replace(/^\/(Users|home)\/[^/]+/, '~');
}

// Mirrors the helper in NewSessionDialog — kept local instead of shared
// because the two dialogs are otherwise independent and the function is
// 6 lines.
function extractClaudeSessionId(args: string[]): string | null {
  for (let i = 0; i < args.length - 1; i++) {
    if (args[i] === '--session-id' || args[i] === '--resume') {
      return args[i + 1] ?? null;
    }
  }
  return null;
}

export function ResumeDialog({
  onClose,
  onResumed,
  onStartNew,
  preferredProviderId,
  preferredSourceSessionId,
  preferredHistoryId,
  preferredDestinationProvider
}: Props): JSX.Element {
  const refresh = useSessions((s) => s.refresh);
  const openSessions = useSessions((s) => s.sessions);

  const [resumable, setResumable] = useState<ResumableSession[] | null>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [partialResult, setPartialResult] = useState<AdoptConversationResult | null>(null);
  const [view, setView] = useState<ViewMode>(readViewMode);
  const [providerFilter, setProviderFilter] = useState<ProviderFilter>('all');
  const [query, setQuery] = useState('');
  const [selected, setSelected] = useState<ResumableSession | null>(null);
  const [destinationProvider, setDestinationProvider] = useState<'claude' | 'codex'>('claude');
  const [runtimeMode, setRuntimeMode] = useState<'rich' | 'terminal'>('rich');
  const preferredDestinationApplied = useRef(false);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    void fetchResumableSessions()
      .then((s) => {
        if (!alive) return;
        let available = s;
        if (preferredProviderId) {
          let preferred = available.find((session) => (
            session.sessionId === preferredProviderId
            && (!preferredHistoryId || session.historyId === preferredHistoryId)
          )) ?? null;
          if (!preferred && preferredSourceSessionId) {
            const source = useSessions.getState().sessions.find((session) => session.id === preferredSourceSessionId);
            if (source && (source.tool === 'claude-code' || source.tool === 'codex')) {
              preferred = {
                sessionId: preferredProviderId,
                tool: source.tool === 'claude-code' ? 'claude' : 'codex',
                origin: source.profile ? `${source.profile} profile` : 'Sessions history',
                cwd: source.cwd,
                modifiedAt: source.exitedAt ?? source.lastDataAt,
                firstUserMessage: source.description || source.name || 'Resumable conversation',
                sizeBytes: 0,
                historyId: preferredHistoryId
              };
              available = [preferred, ...available];
            }
          }
          setSelected(preferred);
        }
        setResumable(available);
        setLoading(false);
      })
      .catch(() => { if (alive) { setResumable(null); setLoading(false); } });
    return () => { alive = false; };
  }, [preferredProviderId, preferredSourceSessionId, preferredHistoryId]);

  // Filter out sessions that are already open as sessionsd tabs — picking
  // one would spawn a second `claude --resume <id>` against the live
  // JSONL the open tab is writing to, fighting for the file.
  const openProviderIds = useMemo(() => {
    const ids = new Set<string>();
    for (const s of openSessions) {
      if (s.exited) continue;
      if (s.tool === 'claude-code') {
        const id = extractClaudeSessionId(s.args);
        if (id) ids.add(`claude:${id}`);
      } else if (s.tool === 'codex' && s.conversationId) {
        ids.add(`codex:${s.conversationId}`);
      }
    }
    return ids;
  }, [openSessions]);

  const available = useMemo(() => {
    if (!resumable) return null;
    const q = query.trim().toLowerCase();
    const list = resumable.filter((s) => (
      !openProviderIds.has(`${s.tool}:${s.sessionId}`)
      && (providerFilter === 'all' || s.tool === providerFilter)
    ));
    if (!q) return list;
    return list.filter((s) => {
      const inTitle = s.title?.toLowerCase().includes(q) ?? false;
      const inMsg = s.firstUserMessage?.toLowerCase().includes(q) ?? false;
      const inFolder = shortFolder(s.cwd).toLowerCase().includes(q)
        || s.cwd.toLowerCase().includes(q);
      const inProvider = s.tool.includes(q) || (s.origin?.toLowerCase().includes(q) ?? false);
      return inTitle || inMsg || inFolder || inProvider;
    });
  }, [resumable, openProviderIds, providerFilter, query]);

  const providerCounts = useMemo(() => {
    const counts = { all: 0, claude: 0, codex: 0 };
    for (const session of resumable ?? []) {
      if (openProviderIds.has(`${session.tool}:${session.sessionId}`)) continue;
      counts.all += 1;
      counts[session.tool] += 1;
    }
    return counts;
  }, [resumable, openProviderIds]);

  useEffect(() => {
    if (!selected || !available) return;
    if (!available.some((session) => session.sessionId === selected.sessionId && session.tool === selected.tool)) {
      setSelected(null);
    }
  }, [available, selected]);

  useEffect(() => {
    if (!selected) return;
    if (preferredDestinationProvider && !preferredDestinationApplied.current) {
      preferredDestinationApplied.current = true;
      setDestinationProvider(preferredDestinationProvider);
      return;
    }
    if (!preferredDestinationProvider) setDestinationProvider(selected.tool);
    setRuntimeMode('rich');
  }, [preferredDestinationProvider, selected?.sessionId, selected?.tool]);

  // Flat = newest-first across all folders. Backend already sorts
  // resumable by modifiedAt desc, so we just keep that order.
  const flatList = available ?? [];

  // Grouped = one section per cwd, sections themselves sorted by their
  // most-recent session's modifiedAt.
  const grouped = useMemo(() => {
    if (!available) return [];
    const map = new Map<string, ResumableSession[]>();
    for (const s of available) {
      const arr = map.get(s.cwd) ?? [];
      arr.push(s);
      map.set(s.cwd, arr);
    }
    return [...map.entries()]
      .map(([cwd, items]) => ({ cwd, items }))
      .sort((a, b) => (b.items[0]?.modifiedAt ?? 0) - (a.items[0]?.modifiedAt ?? 0));
  }, [available]);

  const switchView = (next: ViewMode): void => {
    setView(next);
    writeViewMode(next);
  };

  const resume = async (): Promise<void> => {
    if (!selected) return;
    setBusy(true);
    setError(null);
    setPartialResult(null);
    try {
      const matchingSources = openSessions.filter((session) => (
        session.exited
        && providerConversationId(session) === selected.sessionId
        && (selected.tool === 'claude' ? session.tool === 'claude-code' : session.tool === 'codex')
      ));
      const sourceSessionId = preferredSourceSessionId
        ?? (matchingSources.length === 1 ? matchingSources[0]?.id : undefined);
      const result = await adoptConversation(
        selected.sessionId, sourceSessionId, selected.historyId, destinationProvider, runtimeMode
      );
      await refresh();
      onResumed(result.laneId);
      if (result.partial) {
        setPartialResult(result);
        return;
      }
      onClose();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const repairRecords = async (): Promise<void> => {
    if (!partialResult?.repair) return;
    setBusy(true);
    setError(null);
    try {
      const result = await repairAdoption(partialResult.repair);
      await refresh();
      onResumed(result.laneId);
      if (result.partial) {
        setPartialResult(result);
        return;
      }
      setPartialResult(null);
      onClose();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const openCount = resumable
    ? resumable.filter((s) => openProviderIds.has(`${s.tool}:${s.sessionId}`)).length
    : 0;

  return (
    <div className="dialog-backdrop" onClick={onClose}>
      <div
        className="dialog dialog-wide resume-dialog"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-label="Bring an existing conversation into Sessions"
      >
        <header className="resume-dialog-head">
          <div className="resume-dialog-title-row">
            <div>
              <span className="dialog-kicker">Your Claude and Codex history</span>
              <h2 className="resume-dialog-title">Continue an earlier chat</h2>
            </div>
            <div className="resume-dialog-view-toggle" role="tablist" aria-label="Sort order">
              <button
                type="button"
                role="tab"
                aria-selected={view === 'flat'}
                className={`resume-view-btn${view === 'flat' ? ' is-active' : ''}`}
                onClick={() => switchView('flat')}
                title="Newest first across all folders"
              >Recent</button>
              <button
                type="button"
                role="tab"
                aria-selected={view === 'grouped'}
                className={`resume-view-btn${view === 'grouped' ? ' is-active' : ''}`}
                onClick={() => switchView('grouped')}
                title="One section per folder"
              >By folder</button>
            </div>
          </div>
          <input
            type="text"
            className="resume-dialog-search"
            placeholder={openCount > 0
              ? `Search titles, requests, or workspaces (${openCount} already open)…`
              : 'Search titles, requests, or workspaces…'}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            autoFocus
          />
          <div className="resume-provider-filters" role="tablist" aria-label="Agent">
            {(['all', 'claude', 'codex'] as const).map((provider) => (
              <button
                type="button"
                role="tab"
                aria-selected={providerFilter === provider}
                className={providerFilter === provider ? 'is-active' : ''}
                onClick={() => setProviderFilter(provider)}
                key={provider}
              >
                {provider === 'all' ? 'All chats' : provider === 'claude' ? 'Claude' : 'Codex'}
                <span>{providerCounts[provider]}</span>
              </button>
            ))}
          </div>
        </header>

        <div className="resume-dialog-body">
          <div className="resume-safety-note">
            <strong>Continue the same chat.</strong>
            <span>Sessions groups every prior runtime under one provider conversation. Chats already open elsewhere are hidden so two apps cannot write to the same history at once.</span>
          </div>
          {loading ? (
            <div className="resume-empty">Loading sessions…</div>
          ) : resumable === null ? (
            <div className="resume-empty">Could not load sessions.</div>
          ) : flatList.length === 0 ? (
            <div className="resume-empty">
              <p>
                {query.trim()
                  ? `No ${providerFilter === 'all' ? 'chats' : providerFilter === 'claude' ? 'Claude chats' : 'Codex chats'} match "${query.trim()}".`
                  : openCount > 0 && (resumable?.length ?? 0) === openCount
                    ? 'All resumable conversations are already open in Sessions.'
                    : 'No prior Claude or Codex conversations found on this machine.'}
              </p>
              <button
                type="button"
                className="btn btn-ghost"
                onClick={onStartNew}
              >
                + Start a new session
              </button>
            </div>
          ) : view === 'flat' ? (
            <div className="resume-cards">
              {flatList.map((s) => (
                <ResumeCard key={`${s.tool}:${s.sessionId}`} session={s} selected={selected?.sessionId === s.sessionId && selected.tool === s.tool} onPick={() => setSelected(s)} disabled={busy} />
              ))}
            </div>
          ) : (
            <div className="resume-grouped">
              {grouped.map((g) => (
                <section key={g.cwd} className="resume-group-section">
                  <header className="resume-group-section-head">
                    <span className="resume-group-section-name">{shortFolder(g.cwd)}</span>
                    <span className="resume-group-section-path" title={g.cwd}>{displayPath(g.cwd)}</span>
                    <span className="resume-group-section-count">
                      {g.items.length} session{g.items.length === 1 ? '' : 's'}
                    </span>
                  </header>
                  <div className="resume-cards">
                    {g.items.map((s) => (
                      <ResumeCard
                        key={`${s.tool}:${s.sessionId}`}
                        session={s}
                        selected={selected?.sessionId === s.sessionId && selected.tool === s.tool}
                        onPick={() => setSelected(s)}
                        disabled={busy}
                        hideFolder
                      />
                    ))}
                  </div>
                </section>
              ))}
            </div>
          )}
        </div>

        {error ? <div className="dialog-error">{error}</div> : null}
        {partialResult ? (
          <div className="dialog-warning" role="status" aria-live="assertive">
            <div>
              <strong>
                {partialResult.repair
                  ? 'Resume is live; its records need repair.'
                  : 'The new conversation is live.'}
              </strong>
              <span>{partialResult.warning}</span>
              {partialResult.missingAnnotations?.length ? (
                <span>Missing: {partialResult.missingAnnotations.join(', ')}.</span>
              ) : null}
            </div>
            {partialResult.repair ? (
              <button
                type="button"
                className="btn btn-primary"
                onClick={() => void repairRecords()}
                disabled={busy}
              >
                {busy ? 'Repairing records…' : 'Repair records — do not start another session'}
              </button>
            ) : (
              <button type="button" className="btn btn-primary" onClick={onClose}>
                View conversation
              </button>
            )}
          </div>
        ) : null}
        <footer className="resume-dialog-foot">
          <button type="button" className="btn btn-ghost" onClick={onStartNew} disabled={busy}>
            + New session instead
          </button>
          <button type="button" className="btn btn-ghost" onClick={onClose} disabled={busy}>
            Close
          </button>
          {selected ? (
            <div className="resume-destination">
              <span>Continue with</span>
              <div role="radiogroup" aria-label="Destination agent">
                <button
                  type="button"
                  role="radio"
                  aria-checked={destinationProvider === 'claude'}
                  className={destinationProvider === 'claude' ? 'is-active' : ''}
                  onClick={() => { setDestinationProvider('claude'); if (selected.tool !== 'claude') setRuntimeMode('rich'); }}
                  disabled={busy}
                >Claude</button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={destinationProvider === 'codex'}
                  className={destinationProvider === 'codex' ? 'is-active' : ''}
                  onClick={() => { setDestinationProvider('codex'); if (selected.tool !== 'codex') setRuntimeMode('rich'); }}
                  disabled={busy}
                >Codex</button>
              </div>
              <small>
                {destinationProvider === selected.tool
                  ? 'Resumes the original provider conversation.'
                  : destinationProvider === 'codex'
                    ? 'Creates a Codex chat with authored history imported.'
                    : 'Creates a Claude chat linked to the exact searchable history.'}
              </small>
              <div role="radiogroup" aria-label="Runtime">
                <button
                  type="button"
                  role="radio"
                  aria-checked={runtimeMode === 'rich'}
                  className={runtimeMode === 'rich' ? 'is-active' : ''}
                  onClick={() => setRuntimeMode('rich')}
                  disabled={busy}
                >Rich</button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={runtimeMode === 'terminal'}
                  className={runtimeMode === 'terminal' ? 'is-active' : ''}
                  onClick={() => setRuntimeMode('terminal')}
                  disabled={busy || destinationProvider !== selected.tool}
                  title={destinationProvider !== selected.tool ? 'Cross-provider continuation uses Rich mode' : 'Open the provider’s full terminal interface'}
                >Terminal</button>
              </div>
            </div>
          ) : null}
          <button type="button" className="btn btn-primary" onClick={() => void resume()} disabled={busy || !selected || Boolean(partialResult)}>
            {partialResult
              ? 'Live successor opened'
              : busy
                ? 'Continuing…'
                : selected
                  ? `Continue with ${destinationProvider === 'codex' ? 'Codex' : 'Claude'}`
                  : 'Choose a conversation'}
          </button>
        </footer>
      </div>
    </div>
  );
}

interface CardProps {
  session: ResumableSession;
  selected: boolean;
  onPick: () => void;
  disabled: boolean;
  // In grouped view the folder is already in the section header, so we
  // hide the per-card folder label and let the message column expand.
  hideFolder?: boolean;
}

function ResumeCard({ session, selected, onPick, disabled, hideFolder }: CardProps): JSX.Element {
  const msg = session.firstUserMessage?.trim() || '(no user input yet)';
  const title = session.title?.trim() || msg;
  const hasSeparateRequest = Boolean(session.title?.trim() && msg !== title);
  const size = session.sizeBytes < 100_000
    ? `${Math.round(session.sizeBytes / 1024)} KB`
    : `${(session.sizeBytes / 1024 / 1024).toFixed(1)} MB`;
  return (
    <button
      type="button"
      className={`resume-card${hideFolder ? ' resume-card-no-folder' : ''}${selected ? ' is-selected' : ''}`}
      onClick={onPick}
      disabled={disabled}
      aria-pressed={selected}
      title={`${session.sessionId} · ${size}`}
    >
      <span className="resume-card-provider"><ProviderBadge provider={session.tool} compact /></span>
      <div className="resume-card-content">
        <strong className="resume-card-title">{title}</strong>
        {hasSeparateRequest ? <span className="resume-card-msg">{msg}</span> : null}
        <span className="resume-card-meta">
          {!hideFolder ? <span>{shortFolder(session.cwd)}</span> : null}
          <span>{relativeWhen(session.modifiedAt)}</span>
          {session.origin && session.origin !== 'Claude Code' && session.origin !== 'Codex' ? <span>{session.origin}</span> : null}
        </span>
        {session.runs && session.runs.length > 0 ? (
          <span className="resume-card-chain">
            {session.runs.length === 1
              ? '1 Sessions runtime'
              : `${session.runs.length} linked Sessions runtimes`}
            {session.runs.some((run) => run.movedFromSessionId || run.movedToSessionId) ? ' · continued across machines' : ''}
          </span>
        ) : null}
        {session.promptHistoryOnly ? (
          <span className="resume-card-chain">Claude prompt index · Claude will restore the full chat if the provider still retains it</span>
        ) : null}
      </div>
      <span className="resume-card-choice" aria-hidden>{selected ? '✓' : '›'}</span>
    </button>
  );
}
