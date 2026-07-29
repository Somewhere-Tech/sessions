import { useEffect, useMemo, useState } from 'react';
import { useSessions } from '../store/sessions';
import { DirectoryBrowser } from './DirectoryBrowser';
import { fetchClaudeSettings, fetchProfiles, fetchResumableSessions, listDirectories, sendInput, type AccountProfile, type ResumableSession } from '../api/sessionsd';
import { readNewSessionDefaults, type NewSessionTool } from '../lib/newSessionDefaults';
import { randomUUID } from '../lib/uuid';
import { TagEditor } from './TagEditor';
import type { ClaudeSessionOptions, ClaudeSettings, DirectoryCandidate, SessionInfo } from '../types';
import { getActiveServer } from '../lib/servers';
import { sessionLabel } from '../lib/tabLabels';
import { ProviderMark } from './ProviderBadge';

interface ToolDef {
  id: NewSessionTool;
  name: string;
  description: string;
}

type RuntimeMode = 'rich' | 'terminal';

function defaultRuntimeMode(tool: NewSessionTool, parent: SessionInfo | null, fullAccess: boolean): RuntimeMode {
  if (tool === 'shell') return 'terminal';
  if (parent && parent.tool === tool) {
    return parent.kind === 'codex-app-server' || parent.kind === 'claude-structured' ? 'rich' : 'terminal';
  }
  if (tool === 'claude-code') return 'rich';
  // Codex Rich cannot present app-server approval prompts yet. Until that UI
  // exists, only an explicit saved Full Access choice may default into Rich.
  return tool === 'codex' && fullAccess ? 'rich' : 'terminal';
}

function relativeWhen(ms: number): string {
  const diff = Date.now() - ms;
  if (diff < 60_000) return 'just now';
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`;
  if (diff < 604_800_000) return `${Math.floor(diff / 86_400_000)}d ago`;
  return new Date(ms).toLocaleDateString();
}

const TOOLS: ToolDef[] = [
  { id: 'claude-code', name: 'Claude', description: 'Best for complex reasoning and nuanced responses.' },
  { id: 'codex', name: 'Codex', description: 'Best for codebase tasks, refactoring, and automation.' },
  { id: 'shell', name: 'Shell', description: 'Best for running commands and system operations.' }
];

function workspaceKind(kind: DirectoryCandidate['kind']): string {
  if (kind === 'project') return 'Recent project';
  if (kind === 'home') return 'Home folder';
  return 'Recent workspace';
}

function fallbackWorkspaces(path: string): DirectoryCandidate[] {
  const inferredHome = /^(?:\/Users|\/home)\/[^/]+/.exec(path)?.[0];
  const home = inferredHome ?? path.trim();
  if (!home) return [];
  return [
    { path: home, label: '~', kind: 'home' },
    { path: `${home}/Desktop`, label: '~/Desktop', kind: 'common' },
    { path: `${home}/Documents`, label: '~/Documents', kind: 'common' }
  ];
}

function AgentMark({ tool }: { tool: NewSessionTool }): JSX.Element {
  if (tool === 'claude-code') return <ProviderMark provider="claude" size={38} />;
  if (tool === 'codex') return <ProviderMark provider="codex" size={38} />;
  return <span className="provider-mark is-shell" aria-hidden>$</span>;
}

const NEW_PROFILE = '__new_profile__';
const PROFILE_NAME = /^[a-z0-9-]{1,32}$/;

function providerForTool(tool: NewSessionTool): 'claude' | 'codex' | null {
  return tool === 'claude-code' ? 'claude' : tool === 'codex' ? 'codex' : null;
}

function inheritedProfile(parent: SessionInfo | null, tool: NewSessionTool): string {
  if (!parent?.profile) return '';
  const parentTool: NewSessionTool = parent.tool === 'terminal' ? 'shell' : parent.tool;
  return providerForTool(parentTool) === providerForTool(tool) ? parent.profile : '';
}

async function submitInitialRequest(sessionId: string, text: string): Promise<void> {
  // Match the proven composer path exactly. Ink-based TUIs can buffer a
  // carriage return when it arrives in the same PTY write as pasted text,
  // leaving the request unsent until the next keystroke. A bracketed paste
  // followed by a separate Enter avoids that ambiguity.
  await sendInput(sessionId, `\x1b[200~${text}\x1b[201~`);
  await new Promise<void>((resolve) => window.setTimeout(resolve, 30));
  await sendInput(sessionId, '\r');
}

interface Props {
  onClose: () => void;
  // Creation and opening are separate product actions. The daemon owns the
  // durable session; App owns which views are open.
  onStarted: (sessionId: string) => void;
  // Open the dedicated audited Resume flow. An exact provider id may be
  // supplied, but the dialog still performs the durable adopt.
  onOpenResume?: (providerId?: string) => void;
  parentSession?: SessionInfo | null;
}

// Resolve the (cmd, args) sessionsd should spawn for the selected tool.
// Claude runtime choices are resolved centrally by sessionsd from typed
// settings plus the request's `claude` overrides. Codex retains its existing
// explicit full-access/sandbox choice here.
//
// For Claude Code we always pass `--session-id <uuid>` so Claude uses
// the exact session ID we control. Two big benefits:
//   1. No auto-resume picker. Claude's default behavior in a cwd with
//      prior sessions is to show its resume picker, which fills the
//      buffer with options and feels like "the page never stops
//      loading." Pinning a fresh uuid skips that — Claude starts
//      brand new.
//   2. The JSONL filename is deterministic (<uuid>.jsonl), so the
//      JSONL watcher in sessionsd doesn't have to guess via mtime /
//      birthtime heuristics — it reads exactly our file.
//
// Resume case (when the caller passes a resumeSessionId) uses
// `--resume <id>` so Claude continues that conversation. No fresh
// uuid in that path.
function resolveCommand(
  tool: NewSessionTool,
  skipPerms: boolean,
  resumeSessionId: string | null,
  codexModel: string,
  codexEffort: string,
  claudeSafeMode: boolean
): { cmd: string | undefined; args: string[] | undefined } {
  if (tool === 'claude-code') {
    const args: string[] = [];
    if (resumeSessionId) {
      args.push('--resume', resumeSessionId);
    } else {
      args.push('--session-id', randomUUID());
    }
    if (claudeSafeMode) args.push('--safe-mode');
    return { cmd: 'claude', args };
  }
  if (tool === 'codex') {
    // Full Access is explicit and maps to Codex's exact no-sandbox,
    // no-approval flag. The public default remains workspace-write with
    // on-request approvals.
    const args = skipPerms
        ? ['--dangerously-bypass-approvals-and-sandbox']
        : ['--sandbox', 'workspace-write', '--ask-for-approval', 'on-request'];
    if (codexModel.trim()) args.push('--model', codexModel.trim());
    if (codexEffort) args.push('-c', `model_reasoning_effort="${codexEffort}"`);
    return { cmd: 'codex', args };
  }
  // shell — let sessionsd default to $SHELL
  return { cmd: undefined, args: undefined };
}

// Pull the Claude session id out of a sessionsd session's args. Claude
// is always launched with either `--session-id <uuid>` (fresh start,
// see resolveCommand) or `--resume <uuid>` (resumed conversation).
// Either way, that uuid IS the conversation id Claude writes to
// <uuid>.jsonl — which is what the resume picker enumerates. We use
// this to hide already-open sessions from the inline hint so the user
// doesn't accidentally open a second window onto the same JSONL.
function extractClaudeSessionId(args: string[]): string | null {
  for (let i = 0; i < args.length - 1; i++) {
    if (args[i] === '--session-id' || args[i] === '--resume') {
      return args[i + 1] ?? null;
    }
  }
  return null;
}

export function NewSessionDialog({ onClose, onStarted, onOpenResume, parentSession = null }: Props): JSX.Element {
  const create = useSessions((s) => s.create);
  const openSessions = useSessions((s) => s.sessions);
  const activeId = useSessions((s) => s.activeId);
  const [initialDefaults] = useState(readNewSessionDefaults);
  const [tool, setTool] = useState<NewSessionTool>(() => parentSession?.tool === 'terminal' ? 'shell' : parentSession?.tool ?? initialDefaults.tool);
  const [runtimeMode, setRuntimeMode] = useState<RuntimeMode>(() => defaultRuntimeMode(
    parentSession?.tool === 'terminal' ? 'shell' : parentSession?.tool ?? initialDefaults.tool,
    parentSession,
    initialDefaults.skipPerms
  ));
  const [skipPerms, setSkipPerms] = useState(initialDefaults.skipPerms);
  const [claudeOptions, setClaudeOptions] = useState<ClaudeSessionOptions>({});
  const [claudeDefaults, setClaudeDefaults] = useState<ClaudeSettings | null>(null);
  const [claudeSafeMode, setClaudeSafeMode] = useState(false);
  const [codexModel, setCodexModel] = useState('');
  const [codexEffort, setCodexEffort] = useState('');
  const [cwd, setCwd] = useState(
    parentSession?.cwd
      ?? openSessions.find((session) => session.id === activeId)?.cwd
      ?? initialDefaults.cwd
  );
  const [browserOpen, setBrowserOpen] = useState(false);
  const [tags, setTags] = useState<Record<string, string>>(parentSession?.tags ?? initialDefaults.tags);
  const [task, setTask] = useState('');
  const [recentWorkspaces, setRecentWorkspaces] = useState<DirectoryCandidate[]>([]);
  const [makeWorktree, setMakeWorktree] = useState(false);
  const [profiles, setProfiles] = useState<AccountProfile[]>([]);
  const [profileChoice, setProfileChoice] = useState(() => inheritedProfile(parentSession, tool));
  const [newProfile, setNewProfile] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [createdWithDeliveryError, setCreatedWithDeliveryError] = useState<string | null>(null);

  // Resumable sessions on disk. Loaded once when the dialog opens.
  // Only used now to power the inline "you have prior sessions here"
  // hint; the real picker lives in ResumeDialog.
  const [resumable, setResumable] = useState<ResumableSession[] | null>([]);

  const profileTool = providerForTool(tool);
  const toolProfiles = profiles.filter((profile) => profile.tool === profileTool);
  const selectedProfile = profileChoice === NEW_PROFILE ? newProfile.trim() : profileChoice;
  const profileValid = profileChoice !== NEW_PROFILE || PROFILE_NAME.test(selectedProfile);
  const requiresProviderLogin = profileChoice === NEW_PROFILE;

  useEffect(() => {
    if (parentSession) return;
    let active = true;
    void listDirectories().then((items) => {
      if (!active) return;
      setRecentWorkspaces(items);
      const home = items.find((item) => item.kind === 'home')?.path;
      setCwd((current) => {
        if (current && current !== home) return current;
        return items.find((item) => item.kind === 'project')?.path
          || items.find((item) => item.kind === 'common')?.path
          || current;
      });
    }).catch(() => { if (active) setRecentWorkspaces([]); });
    return () => { active = false; };
  }, [parentSession]);

  useEffect(() => {
    if (!profileTool) {
      setProfileChoice('');
      return;
    }
    const controller = new AbortController();
    void fetchProfiles(controller.signal)
      .then(setProfiles)
      .catch(() => setProfiles([]));
    return () => controller.abort();
  }, [profileTool]);

  useEffect(() => {
    if (tool !== 'claude-code') return;
    const controller = new AbortController();
    void fetchClaudeSettings(controller.signal)
      .then(setClaudeDefaults)
      .catch(() => setClaudeDefaults(null));
    return () => controller.abort();
  }, [tool]);

  useEffect(() => {
    setProfileChoice(inheritedProfile(parentSession, tool));
    setNewProfile('');
  }, [parentSession?.id, parentSession?.profile, parentSession?.tool, tool]);

  useEffect(() => {
    if (tool !== 'claude-code' || selectedProfile !== '') {
      setResumable([]);
      return;
    }
    let alive = true;
    void fetchResumableSessions()
      .then((s) => { if (alive) setResumable(s); })
      .catch(() => { if (alive) setResumable(null); });
    return () => { alive = false; };
  }, [tool, selectedProfile]);

  // Sessions already open as sessionsd tabs — exclude these from the
  // inline hint so we don't suggest resuming what's already on screen.
  const openClaudeIds = useMemo(() => {
    const ids = new Set<string>();
    for (const s of openSessions) {
      if (s.tool !== 'claude-code') continue;
      const id = extractClaudeSessionId(s.args);
      if (id) ids.add(id);
    }
    return ids;
  }, [openSessions]);

  // Sessions specifically inside the currently-selected cwd that are
  // NOT already open — drives the inline resume hint.
  const sessionsForCwd = useMemo(() => {
    if (!resumable || !cwd.trim()) return [];
    const target = cwd.trim();
    return resumable.filter((s) => s.cwd === target && !openClaudeIds.has(s.sessionId));
  }, [resumable, cwd, openClaudeIds]);
  const displayedWorkspaces = useMemo(
    () => {
      const candidates = recentWorkspaces.length > 0
        ? recentWorkspaces
        : fallbackWorkspaces(initialDefaults.cwd || cwd);
      const safer = candidates.filter((item) => item.kind !== 'home');
      const selected = cwd && candidates.every((item) => item.path !== cwd)
        ? {
            path: cwd,
            label: cwd.split('/').filter(Boolean).slice(-1)[0] ?? cwd,
            kind: 'project' as const
          }
        : candidates.find((item) => item.path === cwd);
      return [...(selected ? [selected] : []), ...safer]
        .filter((item) => item.kind !== 'home' || item.path === cwd)
        .filter((item, index, items) => items.findIndex((candidate) => candidate.path === item.path) === index)
        .slice(0, 3);
    },
    [recentWorkspaces, initialDefaults.cwd, cwd]
  );
  const homeWorkspace = useMemo(
    () => recentWorkspaces.find((item) => item.kind === 'home')?.path
      ?? fallbackWorkspaces(initialDefaults.cwd || cwd).find((item) => item.kind === 'home')?.path
      ?? '',
    [recentWorkspaces, initialDefaults.cwd, cwd]
  );

  const startSession = async (resumeId: string | null): Promise<void> => {
    if (!profileValid) {
      setError('Profile names use 1–32 lowercase letters, numbers, or hyphens.');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const { cmd, args } = resolveCommand(tool, skipPerms, resumeId, codexModel, codexEffort, claudeSafeMode);
      const resumeCwd = resumeId
        ? (resumable?.find((s) => s.sessionId === resumeId)?.cwd ?? cwd.trim())
        : cwd.trim();
      const info = await create({
        cmd,
        args,
        kind: tool === 'codex' && runtimeMode === 'rich'
          ? 'codex-app-server'
          : tool === 'claude-code' && runtimeMode === 'rich'
          ? 'claude-structured'
          : undefined,
        cwd: resumeCwd || undefined,
        cols: initialDefaults.cols,
        rows: initialDefaults.rows,
        name: task.trim() ? task.trim().split('\n')[0]?.slice(0, 80) : undefined,
        description: task.trim() || undefined,
        tags,
        profile: selectedProfile || undefined,
        worktree: !parentSession && makeWorktree,
        // A newly isolated provider home starts in its login flow. Readiness
        // cannot distinguish that prompt from the agent composer, so never
        // inject an initial task until the user has authenticated explicitly.
        waitReady: task.trim().length > 0 && !requiresProviderLogin,
        claude: tool === 'claude-code'
          ? claudeSafeMode
            ? { ...claudeOptions, remoteControl: 'off', chrome: 'off', somewhereMcp: 'inherit' }
            : claudeOptions
          : undefined,
        creatorSessionId: parentSession?.id
      });
      // Open the durable record before attempting prompt delivery. Permission
      // dialogs and provider readiness are runtime concerns; they must not
      // strand a successfully-created session behind the launcher.
      onStarted(info.id);
      if (task.trim()) {
        if (requiresProviderLogin) {
          setCreatedWithDeliveryError(info.id);
          setError(`Session ${info.id.slice(0, 8)} started in the provider login flow. Finish authentication first, then send the request shown above from Conversation. Sessions will not queue or paste it into a login prompt.`);
          return;
        }
        try {
          await submitInitialRequest(info.id, task.trim());
        } catch (reason) {
          setCreatedWithDeliveryError(info.id);
          setError(`Session ${info.id.slice(0, 8)} started, but Sessions could not confirm its first request: ${(reason as Error).message}. Open the session and inspect the terminal before typing anything else; the request may be waiting for one Enter.`);
          return;
        }
      }
      onClose();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const submit = (e: React.FormEvent): void => {
    e.preventDefault();
    void startSession(null);
  };

  const showSkipPerms = tool === 'codex';

  const isDelegate = parentSession !== null;

  return (
    <div className="dialog-backdrop" onClick={onClose}>
      <form className="dialog dialog-wide new-session-launcher" onClick={(e) => e.stopPropagation()} onSubmit={submit}>
        <header className="dialog-head">
          <div><span className="dialog-kicker">{isDelegate ? `Linked to ${parentSession ? sessionLabel(parentSession) : 'this session'}` : 'Start a conversation or command'}</span><h2 className="dialog-title">New Session</h2></div>
          <div className="launcher-head-actions">
            {onOpenResume && !isDelegate && selectedProfile === '' ? (
              <button type="button" className="dialog-head-link" onClick={() => onOpenResume()}>Resume instead</button>
            ) : null}
            <button type="button" className="launcher-close" onClick={onClose} aria-label="Close new session">×</button>
          </div>
        </header>
        <div className="dialog-body">
          <div className="field launcher-task-field">
            <span className="field-label">What do you want to work on? <span className="field-optional">optional</span></span>
            <textarea value={task} onChange={(event) => setTask(event.currentTarget.value)} placeholder={isDelegate ? 'What should this linked session work on?' : 'Describe a task, ask a question, or leave blank to start…'} rows={3} autoFocus />
            <span className="field-help">{requiresProviderLogin ? 'A new account must finish login first. This request will stay here for copying; Sessions will not queue or paste it into the login flow.' : isDelegate ? `Starts in ${parentSession?.cwd} on this machine and stays grouped under ${parentSession ? sessionLabel(parentSession) : 'the current session'}.` : 'Sent once the provider is ready. Sessions does not create a hidden prompt queue.'}</span>
          </div>
          {isDelegate ? (
            <div className="launcher-inherited"><span>Workspace</span><strong>{cwd}</strong><small>{getActiveServer().name} · parent {parentSession?.id.slice(0, 8)}</small></div>
          ) : (
            <div className="field launcher-workspace-field">
              <div className="launcher-section-head">
                <span className="field-label">Workspace</span>
                <details className="workspace-browser-disclosure" open={browserOpen} onToggle={(event) => setBrowserOpen(event.currentTarget.open)}>
                  <summary>Choose folder…</summary>
                  {browserOpen ? (
                    <div className="workspace-browser-panel">
                      <input className="field-input workspace-path-input" value={cwd} onChange={(event) => setCwd(event.currentTarget.value)} placeholder="/path/to/project" />
                      <DirectoryBrowser value={cwd} onChange={(path, confirmed) => { setCwd(path); if (confirmed) setBrowserOpen(false); }} />
                    </div>
                  ) : null}
                </details>
              </div>
              {displayedWorkspaces.length > 0 ? <div className="recent-workspaces">{displayedWorkspaces.map((item) => <button type="button" key={item.path} className={cwd === item.path ? 'is-active' : ''} onClick={() => setCwd(item.path)}><span className="workspace-folder-icon" aria-hidden /><span className="workspace-card-copy"><strong>{item.label}</strong><small>{getActiveServer().name} · {workspaceKind(item.kind)}</small></span><span className="workspace-radio" aria-hidden /></button>)}</div> : null}
              <div className="workspace-selection"><span className="workspace-status-dot" aria-hidden /><strong>{getActiveServer().name}</strong><span>·</span><code>{cwd || 'Choose a workspace'}</code></div>
              {cwd === homeWorkspace ? <span className="field-help">Your whole home folder includes protected locations such as Music and cloud drives. Choose a project folder to avoid extra macOS permission prompts.</span> : null}
            </div>
          )}
          <div className="field launcher-agent-field">
            <span className="field-label">Agent</span>
            <div className="tool-selector">
              {TOOLS.map((t) => (
                <button key={t.id} type="button" className={`tool-option${tool === t.id ? ' is-active' : ''}`} onClick={() => {
                  setTool(t.id);
                  setRuntimeMode(defaultRuntimeMode(t.id, parentSession, skipPerms));
                }}>
                  <AgentMark tool={t.id} />
                  <span className="tool-choice-radio" aria-hidden />
                  <span className="tool-name">{t.name}</span>
                  <span className="tool-description">{t.description}</span>
                </button>
              ))}
            </div>
          </div>
          {tool !== 'shell' ? (
            <div className="field launcher-runtime-field">
              <span className="field-label">Experience</span>
              <div className="runtime-mode-selector" role="radiogroup" aria-label="Agent runtime experience">
                <button type="button" role="radio" aria-checked={runtimeMode === 'rich'} className={`runtime-mode-option${runtimeMode === 'rich' ? ' is-active' : ''}`} onClick={() => {
                  setRuntimeMode('rich');
                  if (tool === 'codex') setSkipPerms(true);
                }}>
                  <span><strong>Rich</strong><em>{tool === 'codex' ? 'Full access' : 'Recommended'}</em></span>
                  <small>{tool === 'codex' ? 'Native Codex app-server events and model controls. Currently requires Full Access because approval prompts are not available here yet.' : 'Structured Claude turns without screen parsing. Slash commands and interactive pickers are not available yet.'}</small>
                </button>
                <button type="button" role="radio" aria-checked={runtimeMode === 'terminal'} className={`runtime-mode-option${runtimeMode === 'terminal' ? ' is-active' : ''}`} onClick={() => setRuntimeMode('terminal')}>
                  <span><strong>Terminal</strong><em>Compatible</em></span>
                  <small>Runs the provider’s exact terminal UI. Best for slash commands and pickers; status and message parsing can be incomplete.</small>
                </button>
              </div>
              {tool === 'claude-code' && runtimeMode === 'rich' ? <span className="field-help">Claude Rich currently uses the subscription-authenticated structured CLI path, not the Agent SDK. If it cannot start, Sessions reports the failure and does not silently switch runtimes.</span> : null}
            </div>
          ) : null}
          {tool !== 'shell' ? (
            <div className="launcher-model-controls" aria-label={`${tool === 'claude-code' ? 'Claude' : 'Codex'} model and effort`}>
              <label>
                <span>Model</span>
                <input
                  value={tool === 'claude-code' ? (claudeOptions.model ?? '') : codexModel}
                  maxLength={128}
                  placeholder={tool === 'claude-code' && claudeDefaults?.model ? `Default · ${claudeDefaults.model}` : 'Provider default'}
                  onChange={(event) => {
                    if (tool === 'claude-code') setClaudeOptions((current) => ({ ...current, model: event.currentTarget.value }));
                    else setCodexModel(event.currentTarget.value);
                  }}
                />
              </label>
              <label>
                <span>Effort</span>
                <select
                  value={tool === 'claude-code' ? (claudeOptions.effort ?? '') : codexEffort}
                  onChange={(event) => {
                    if (tool === 'claude-code') setClaudeOptions((current) => ({ ...current, effort: event.currentTarget.value as ClaudeSessionOptions['effort'] }));
                    else setCodexEffort(event.currentTarget.value);
                  }}
                >
                  <option value="">{tool === 'claude-code' && claudeDefaults && claudeDefaults.effort !== 'inherit' ? `Settings default · ${claudeDefaults.effort}` : 'Provider default'}</option>
                  <option value="low">Low</option>
                  <option value="medium">Medium</option>
                  <option value="high">High</option>
                  <option value="xhigh">Extra high</option>
                  <option value="max">Max</option>
                </select>
              </label>
            </div>
          ) : null}
          <details className="launcher-advanced">
            <summary>More options <span>account · tags · permissions · worktree</span></summary>
          {profileTool ? (
            <div className="field account-profile-field">
              <span className="field-label">Account <span className="field-optional">optional · separate login</span></span>
              <select
                className="field-input"
                value={profileChoice}
                onChange={(event) => setProfileChoice(event.target.value)}
                disabled={busy}
              >
                <option value="">Default {tool === 'claude-code' ? 'Claude' : 'Codex'} login</option>
                {selectedProfile && profileChoice !== NEW_PROFILE && !toolProfiles.some((profile) => profile.name === selectedProfile) ? (
                  <option value={selectedProfile}>{selectedProfile} · inherited</option>
                ) : null}
                {toolProfiles.map((profile) => (
                  <option key={`${profile.tool}:${profile.name}`} value={profile.name}>{profile.name}</option>
                ))}
                <option value={NEW_PROFILE}>Add another login…</option>
              </select>
              {profileChoice === NEW_PROFILE ? (
                <input
                  className="field-input"
                  value={newProfile}
                  onChange={(event) => setNewProfile(event.target.value.toLowerCase())}
                  placeholder="work or personal"
                  maxLength={32}
                  pattern="[a-z0-9-]{1,32}"
                  autoFocus
                  aria-invalid={!profileValid}
                />
              ) : null}
              <span className="field-help">
                Profiles keep provider credentials and histories separate. A new profile opens the provider's own login flow.
              </span>
            </div>
          ) : null}
            <div className="field">
              <span className="field-label">Tags <span className="field-optional">optional</span></span>
              <TagEditor value={tags} onChange={setTags} disabled={busy} />
            </div>
            {!isDelegate ? <label className="field-checkbox"><input type="checkbox" checked={makeWorktree} onChange={(event) => setMakeWorktree(event.currentTarget.checked)} /><span className="field-checkbox-body"><span>Create an isolated worktree</span><span className="field-hint">Uses Sessions' existing safe worktree flow.</span></span></label> : null}
          {tool === 'claude-code' ? (
            <div className="launcher-claude-options">
              <label className="field-checkbox is-wide">
                <input type="checkbox" checked={claudeSafeMode} onChange={(event) => setClaudeSafeMode(event.currentTarget.checked)} />
                <span className="field-checkbox-body">
                  <span>Start without customizations</span>
                  <span className="field-hint">Claude safe mode disables user hooks, plugins, MCP servers, skills, and project instructions for this session. Use it to troubleshoot unexpected macOS permission prompts.</span>
                </span>
              </label>
              <label><span>Permission mode</span><select value={claudeOptions.permissionMode ?? ''} onChange={(event) => setClaudeOptions((current) => ({ ...current, permissionMode: event.currentTarget.value as ClaudeSessionOptions['permissionMode'] }))}>
                <option value="">Settings default</option>
                <option value="inherit">Claude default</option>
                <option value="manual">Manual</option>
                <option value="acceptEdits">Accept edits</option>
                <option value="auto">Auto</option>
                <option value="plan">Plan</option>
                <option value="dontAsk">Don’t ask</option>
                <option value="bypassPermissions">Bypass permissions</option>
              </select></label>
              <label><span>Remote Control</span><select value={claudeOptions.remoteControl ?? ''} onChange={(event) => setClaudeOptions((current) => ({ ...current, remoteControl: event.currentTarget.value as ClaudeSessionOptions['remoteControl'] }))}>
                <option value="">Settings default</option><option value="inherit">Claude default</option><option value="on">On</option><option value="off">Off</option>
              </select></label>
              <label><span>Chrome</span><select value={claudeOptions.chrome ?? ''} onChange={(event) => setClaudeOptions((current) => ({ ...current, chrome: event.currentTarget.value as ClaudeSessionOptions['chrome'] }))}>
                <option value="">Settings default</option><option value="inherit">Claude default</option><option value="on">On</option><option value="off">Off</option>
              </select></label>
              <label><span>Somewhere MCP</span><select value={claudeOptions.somewhereMcp ?? ''} onChange={(event) => setClaudeOptions((current) => ({ ...current, somewhereMcp: event.currentTarget.value as ClaudeSessionOptions['somewhereMcp'] }))}>
                <option value="">Settings default</option><option value="inherit">Use provider configuration</option><option value="ensure">Ensure enabled</option>
              </select></label>
              <label className="is-wide"><span>Remote Control name prefix</span><input value={claudeOptions.remoteControlNamePrefix ?? ''} maxLength={64} placeholder="Settings default" onChange={(event) => setClaudeOptions((current) => ({ ...current, remoteControlNamePrefix: event.currentTarget.value }))} /></label>
            </div>
          ) : null}
          {showSkipPerms ? (
            <label className="field-checkbox">
              <input
                type="checkbox"
                checked={skipPerms}
                onChange={(e) => {
                  const enabled = e.target.checked;
                  setSkipPerms(enabled);
                  if (!enabled && tool === 'codex' && runtimeMode === 'rich') setRuntimeMode('terminal');
                }}
              />
              <span className="field-checkbox-body">
                <span>Full Access</span>
                <span className="field-hint">
                  Runs without Codex approval or sandbox protections. Leave off unless this workspace and task are trusted.
                </span>
              </span>
            </label>
          ) : null}
          </details>
          {/* Inline hints always enter the audited Resume flow. They never
              create an unlinked direct `--resume` runtime. */}
          {tool === 'claude-code' && sessionsForCwd.length > 0 ? (
            sessionsForCwd.length === 1 ? (
              <button
                type="button"
                className="resume-hint"
                onClick={() => onOpenResume?.(sessionsForCwd[0].sessionId)}
                disabled={busy}
                title={sessionsForCwd[0].firstUserMessage ?? ''}
              >
                Resume “{(sessionsForCwd[0].firstUserMessage ?? '(no user input yet)').slice(0, 60)}
                {(sessionsForCwd[0].firstUserMessage ?? '').length > 60 ? '…' : ''}” ({relativeWhen(sessionsForCwd[0].modifiedAt)})
              </button>
            ) : (
              <button
                type="button"
                className="resume-hint"
                onClick={() => onOpenResume?.()}
              >
                {sessionsForCwd.length} prior sessions in this folder · Resume?
              </button>
            )
          ) : null}
          {error ? <div className="dialog-error">{error}</div> : null}
          <div className="dialog-actions">
            <button type="button" className="btn btn-ghost" onClick={onClose} disabled={busy}>Cancel</button>
            <button type={createdWithDeliveryError ? 'button' : 'submit'} className="btn btn-primary" disabled={!createdWithDeliveryError && (busy || !cwd.trim() || !profileValid)} onClick={createdWithDeliveryError ? onClose : undefined}>
              {createdWithDeliveryError ? 'View session' : busy ? 'Starting…' : 'Start session'}
            </button>
          </div>
        </div>
      </form>
    </div>
  );
}
