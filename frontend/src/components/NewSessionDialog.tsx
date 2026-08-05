import { useEffect, useMemo, useState } from 'react';
import { useSessions } from '../store/sessions';
import { DirectoryBrowser } from './DirectoryBrowser';
import {
  fetchProfiles,
  listDirectories,
  listNewSessionCodexModels,
  submitMessage,
  type AccountProfile,
  type SessionModelOption
} from '../api/sessionsd';
import { readNewSessionDefaults, type NewSessionTool } from '../lib/newSessionDefaults';
import { TagEditor } from './TagEditor';
import type { ClaudeSessionOptions, DirectoryCandidate, SessionInfo } from '../types';
import { getActiveServer, isLocalServer, serverDisplayName, useServers } from '../lib/servers';
import { sessionLabel } from '../lib/tabLabels';
import { ProviderMark } from './ProviderBadge';
import { MachineMark } from './MachineMark';
import { CLAUDE_MODEL_OPTIONS, ModelPicker, type ModelPickerOption } from './ModelPicker';

interface ToolDef {
  id: NewSessionTool;
  name: string;
  description: string;
}

type RuntimeMode = 'rich' | 'terminal';

function defaultRuntimeMode(tool: NewSessionTool, parent: SessionInfo | null, fullAccess: boolean): RuntimeMode {
  if (tool === 'shell') return 'terminal';
  // Claude always starts in its native interactive runtime. Conversation,
  // Terminal, claude.ai, and mobile then observe one provider session even
  // when the delegate was created from an older structured parent.
  if (tool === 'claude-code') return 'terminal';
  if (parent && parent.tool === tool) {
    return parent.kind === 'codex-app-server' || parent.kind === 'claude-structured' ? 'rich' : 'terminal';
  }
  // Codex Rich cannot present app-server approval prompts yet. Until that UI
  // exists, only an explicit saved Full Access choice may default into Rich.
  return tool === 'codex' && fullAccess ? 'rich' : 'terminal';
}

function effortLabel(effort: string): string {
  if (effort === 'xhigh') return 'Extra high';
  return effort.charAt(0).toUpperCase() + effort.slice(1);
}

const TOOLS: ToolDef[] = [
  { id: 'claude-code', name: 'Claude', description: 'Deep reasoning and long-running work.' },
  { id: 'codex', name: 'Codex', description: 'Code-focused planning and implementation.' },
  { id: 'shell', name: 'Shell', description: 'Commands without an AI agent.' }
];

function workspaceKind(kind: DirectoryCandidate['kind']): string {
  if (kind === 'somewhere') return 'Somewhere project';
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

function AgentMark({ tool, size = 38 }: { tool: NewSessionTool; size?: number }): JSX.Element {
  if (tool === 'claude-code') return <ProviderMark provider="claude" size={size} />;
  if (tool === 'codex') return <ProviderMark provider="codex" size={size} />;
  return <span className="provider-mark is-shell" style={{ width: size, height: size, fontSize: Math.max(12, size / 2) }} aria-hidden>$</span>;
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

async function submitInitialRequest(sessionId: string, text: string, serverId: string): Promise<void> {
  await submitMessage(sessionId, `\x1b[200~${text}\x1b[201~`, serverId);
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
  embedded?: boolean;
}

// Resolve the (cmd, args) sessionsd should spawn for the selected tool.
// Claude runtime choices are resolved centrally by sessionsd from typed
// settings plus the request's `claude` overrides. Codex retains its existing
// explicit full-access/sandbox choice here.
//
// A New Session never receives a resume identity. sessionsd appends the new
// durable lane ID as Claude's --session-id at the authoritative launch
// boundary, keeping the provider transcript and Sessions record aligned.
function resolveCommand(
  tool: NewSessionTool,
  skipPerms: boolean,
  codexModel: string,
  codexEffort: string,
  claudeSafeMode: boolean
): { cmd: string | undefined; args: string[] | undefined } {
  if (tool === 'claude-code') {
    const args: string[] = [];
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

export function NewSessionDialog({ onClose, onStarted, onOpenResume, parentSession = null, embedded = false }: Props): JSX.Element {
  const create = useSessions((s) => s.create);
  const openSessions = useSessions((s) => s.sessions);
  const activeId = useSessions((s) => s.activeId);
  const sessionsServerId = useSessions((s) => s.serverId);
  const setServerScope = useSessions((s) => s.setServerScope);
  const configuredMachines = useServers((state) => state.servers);
  const activeMachineId = useServers((state) => state.activeId);
  const selectActiveMachine = useServers((state) => state.setActive);
  const [initialDefaults] = useState(readNewSessionDefaults);
  const [machineId, setMachineId] = useState(() => {
    if (parentSession) return activeMachineId ?? configuredMachines[0]?.id ?? '';
    return configuredMachines.find((machine) => machine.isDefault && isLocalServer(machine))?.id
      ?? activeMachineId
      ?? configuredMachines[0]?.id
      ?? '';
  });
  const [tool, setTool] = useState<NewSessionTool>(() => parentSession?.tool === 'terminal' ? 'shell' : parentSession?.tool ?? initialDefaults.tool);
  const [runtimeMode, setRuntimeMode] = useState<RuntimeMode>(() => defaultRuntimeMode(
    parentSession?.tool === 'terminal' ? 'shell' : parentSession?.tool ?? initialDefaults.tool,
    parentSession,
    initialDefaults.skipPerms
  ));
  const [skipPerms, setSkipPerms] = useState(initialDefaults.skipPerms);
  const [claudeOptions, setClaudeOptions] = useState<ClaudeSessionOptions>({});
  const [claudeSafeMode, setClaudeSafeMode] = useState(false);
  const [codexModel, setCodexModel] = useState('');
  const [codexEffort, setCodexEffort] = useState('');
  const [codexModels, setCodexModels] = useState<SessionModelOption[]>([]);
  const [codexModelsLoading, setCodexModelsLoading] = useState(false);
  const [codexModelsError, setCodexModelsError] = useState<string | null>(null);
  const [cwd, setCwd] = useState(
    parentSession?.cwd
      ?? openSessions.find((session) => session.id === activeId)?.cwd
      ?? initialDefaults.cwd
  );
  const [browserOpen, setBrowserOpen] = useState(false);
  const [tags, setTags] = useState<Record<string, string>>(parentSession?.tags ?? initialDefaults.tags);
  const [task, setTask] = useState('');
  const [recentWorkspaces, setRecentWorkspaces] = useState<DirectoryCandidate[]>([]);
  const [profiles, setProfiles] = useState<AccountProfile[]>([]);
  const [profileChoice, setProfileChoice] = useState(() => inheritedProfile(parentSession, tool));
  const [newProfile, setNewProfile] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [createdWithDeliveryError, setCreatedWithDeliveryError] = useState<string | null>(null);
  useEffect(() => {
    const closeOnEscape = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape' || event.defaultPrevented) return;
      event.preventDefault();
      onClose();
    };
    window.addEventListener('keydown', closeOnEscape);
    return () => window.removeEventListener('keydown', closeOnEscape);
  }, [onClose]);

  const profileTool = providerForTool(tool);
  const toolProfiles = profiles.filter((profile) => profile.tool === profileTool);
  const selectedProfile = profileChoice === NEW_PROFILE ? newProfile.trim() : profileChoice;
  const profileValid = profileChoice !== NEW_PROFILE || PROFILE_NAME.test(selectedProfile);
  const requiresProviderLogin = profileChoice === NEW_PROFILE;
  const selectedMachine = configuredMachines.find((machine) => machine.id === machineId)
    ?? configuredMachines[0]
    ?? null;

  useEffect(() => {
    if (parentSession) return;
    if (!configuredMachines.some((machine) => machine.id === machineId)) {
      const fallback = configuredMachines.find((machine) => machine.isDefault && isLocalServer(machine))
        ?? configuredMachines[0];
      if (fallback) setMachineId(fallback.id);
      return;
    }
    selectActiveMachine(machineId);
    setServerScope(machineId);
    let active = true;
    void listDirectories(machineId).then((items) => {
      if (!active) return;
      setRecentWorkspaces(items);
      setCwd((current) => {
        const currentCandidate = items.find((item) => item.path === current);
        if (currentCandidate) return current;
        return items.find((item) => item.kind === 'somewhere')?.path
          || items.find((item) => item.kind === 'project')?.path
          || items.find((item) => item.kind === 'common')?.path
          || items.find((item) => item.kind === 'home')?.path
          || current;
      });
    }).catch(() => { if (active) setRecentWorkspaces([]); });
    return () => { active = false; };
  }, [configuredMachines, machineId, parentSession, selectActiveMachine, setServerScope]);

  useEffect(() => {
    if (!profileTool) {
      setProfileChoice('');
      return;
    }
    const controller = new AbortController();
    void fetchProfiles(controller.signal, machineId)
      .then(setProfiles)
      .catch(() => setProfiles([]));
    return () => controller.abort();
  }, [machineId, profileTool]);

  useEffect(() => {
    if (tool !== 'codex') {
      setCodexModels([]);
      setCodexModelsError(null);
      setCodexModelsLoading(false);
      return;
    }
    const controller = new AbortController();
    setCodexModels([]);
    setCodexModelsError(null);
    setCodexModelsLoading(true);
    void listNewSessionCodexModels(controller.signal, machineId)
      .then((models) => setCodexModels(models.filter((model) => !model.hidden)))
      .catch((reason) => {
        if (!controller.signal.aborted) {
          setCodexModelsError(reason instanceof Error ? reason.message : 'Could not load Codex models.');
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setCodexModelsLoading(false);
      });
    return () => controller.abort();
  }, [machineId, tool]);

  useEffect(() => {
    setProfileChoice(inheritedProfile(parentSession, tool));
    setNewProfile('');
  }, [parentSession?.id, parentSession?.profile, parentSession?.tool, tool]);

  const openSessionWorkspaces = useMemo<DirectoryCandidate[]>(() => {
    if (sessionsServerId !== machineId) return [];
    const seen = new Set<string>();
    return [...openSessions]
      .filter((session) => !session.exited && session.cwd.trim())
      .sort((left, right) => Math.max(right.lastDataAt || 0, right.createdAt || 0) - Math.max(left.lastDataAt || 0, left.createdAt || 0))
      .flatMap((session) => {
        const path = session.cwd.trim();
        if (seen.has(path)) return [];
        seen.add(path);
        return [{
          path,
          label: path.split('/').filter(Boolean).pop() ?? path,
          kind: 'project' as const
        }];
      });
  }, [machineId, openSessions, sessionsServerId]);
  const openSessionWorkspacePaths = useMemo(
    () => new Set(openSessionWorkspaces.map((item) => item.path)),
    [openSessionWorkspaces]
  );
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
      return [...(selected ? [selected] : []), ...openSessionWorkspaces, ...safer]
        .filter((item) => item.kind !== 'home' || item.path === cwd)
        .filter((item, index, items) => items.findIndex((candidate) => candidate.path === item.path) === index)
        .slice(0, 8);
    },
    [recentWorkspaces, initialDefaults.cwd, cwd, openSessionWorkspaces]
  );
  const homeWorkspace = useMemo(
    () => recentWorkspaces.find((item) => item.kind === 'home')?.path
      ?? fallbackWorkspaces(initialDefaults.cwd || cwd).find((item) => item.kind === 'home')?.path
      ?? '',
    [recentWorkspaces, initialDefaults.cwd, cwd]
  );
  const selectedCodexModel = codexModels.find((model) => model.id === codexModel)
    ?? codexModels.find((model) => model.isDefault);
  const modelOptions: ModelPickerOption[] = tool === 'claude-code'
    ? CLAUDE_MODEL_OPTIONS
    : codexModels.map((model) => ({
        id: model.id,
        label: model.displayName || model.id,
        description: model.isDefault
          ? `Default · ${model.defaultReasoningEffort || 'provider effort'}`
          : model.id,
        isDefault: model.isDefault
      }));
  const effortChoices = tool === 'claude-code'
    ? ['low', 'medium', 'high', 'xhigh', 'max']
    : selectedCodexModel?.supportedReasoningEfforts.map((option) => option.reasoningEffort) ?? [];

  const chooseMachine = (nextMachineId: string): void => {
    if (nextMachineId === machineId) return;
    selectActiveMachine(nextMachineId);
    setMachineId(nextMachineId);
    setRecentWorkspaces([]);
    setCwd('');
    setBrowserOpen(false);
  };

  const startSession = async (): Promise<void> => {
    if (!profileValid) {
      setError('Profile names use 1–32 lowercase letters, numbers, or hyphens.');
      return;
    }
    if (!machineId) {
      setError('Choose a computer before starting the session.');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      if (!parentSession) {
        selectActiveMachine(machineId);
        setServerScope(machineId);
      }
      const { cmd, args } = resolveCommand(tool, skipPerms, codexModel, codexEffort, claudeSafeMode);
      const info = await create({
        cmd,
        args,
        kind: tool === 'codex' && runtimeMode === 'rich'
          ? 'codex-app-server'
          : tool === 'claude-code' && runtimeMode === 'rich'
          ? 'claude-structured'
          : undefined,
        cwd: cwd.trim() || undefined,
        cols: initialDefaults.cols,
        rows: initialDefaults.rows,
        name: task.trim() ? task.trim().split('\n')[0]?.slice(0, 80) : undefined,
        description: task.trim() || undefined,
        tags,
        profile: selectedProfile || undefined,
        // A newly isolated provider home starts in its login flow. Readiness
        // cannot distinguish that prompt from the agent composer, so never
        // inject an initial task until the user has authenticated explicitly.
        waitReady: task.trim().length > 0 && !requiresProviderLogin,
        claude: tool === 'claude-code'
          ? claudeSafeMode
            ? { ...claudeOptions, remoteControl: 'off', chrome: 'off', somewhereMcp: 'inherit' }
            : claudeOptions
          : undefined,
        creatorSessionId: parentSession?.id,
        delegationKind: parentSession ? 'user' : undefined
      }, machineId);
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
          await submitInitialRequest(info.id, task.trim(), machineId);
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
    void startSession();
  };

  const isDelegate = parentSession !== null;
  const selectedTool = TOOLS.find((item) => item.id === tool) ?? TOOLS[0];
  const selectedModel = tool === 'claude-code' ? (claudeOptions.model ?? '') : codexModel;
  const selectedEffort = tool === 'claude-code' ? (claudeOptions.effort ?? '') : codexEffort;
  const selectModel = (nextModel: string): void => {
    if (tool === 'claude-code') {
      setClaudeOptions((current) => ({ ...current, model: nextModel }));
      return;
    }
    setCodexModel(nextModel);
    const option = codexModels.find((model) => model.id === nextModel)
      ?? codexModels.find((model) => model.isDefault);
    if (codexEffort && !option?.supportedReasoningEfforts.some((effort) => effort.reasoningEffort === codexEffort)) {
      setCodexEffort('');
    }
  };
  const selectEffort = (nextEffort: string): void => {
    if (tool === 'claude-code') {
      setClaudeOptions((current) => ({ ...current, effort: nextEffort as ClaudeSessionOptions['effort'] }));
      return;
    }
    setCodexEffort(nextEffort);
  };
  const chooseTool = (nextTool: NewSessionTool): void => {
    setTool(nextTool);
    setRuntimeMode(defaultRuntimeMode(nextTool, parentSession, skipPerms));
  };
  const selectedWorkspace = recentWorkspaces.find((item) => item.path === cwd)
    ?? displayedWorkspaces.find((item) => item.path === cwd);
  const workspaceTitle = selectedWorkspace?.label
    ?? cwd.split('/').filter(Boolean).pop()
    ?? 'this project';
  const machineTitle = selectedMachine
    ? serverDisplayName(selectedMachine, true)
    : 'Choose computer';

  const launcher = (
      <form className={`dialog dialog-wide new-session-launcher${embedded ? ' is-embedded' : ''}`} onClick={(e) => e.stopPropagation()} onSubmit={submit}>
        <header className="dialog-head launcher-compact-head">
          <div className="launcher-breadcrumb">
            <span className="workspace-folder-icon" aria-hidden />
            <span>{isDelegate ? sessionLabel(parentSession as SessionInfo) : workspaceTitle}</span>
            <span aria-hidden>/</span>
            <strong>{isDelegate ? 'Linked session' : 'New session'}</strong>
          </div>
          <div className="launcher-head-actions">
            {onOpenResume && !isDelegate && selectedProfile === '' ? (
              <button type="button" className="dialog-head-link" onClick={() => onOpenResume()}>Resume an earlier chat</button>
            ) : null}
            <button type="button" className="launcher-close" onClick={onClose} aria-label="Close new session">×</button>
          </div>
        </header>
        <div className="dialog-body">
          <section className="launcher-hero">
            <span>{isDelegate ? 'Linked session' : 'New session'}</span>
            <h2 className="launcher-intent" aria-label={isDelegate ? 'Start a new linked session' : 'Start a new session'}>
              <span>Start a new</span>
              <label className="launcher-intent-control is-agent" title={`Agent: ${selectedTool.name}`}>
                <AgentMark tool={tool} size={17} />
                <select value={tool} onChange={(event) => chooseTool(event.currentTarget.value as NewSessionTool)} aria-label="Agent">
                  {TOOLS.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}
                </select>
              </label>
              <span>{isDelegate ? 'session linked to' : 'session on'}</span>
              {isDelegate ? (
                <strong>{parentSession ? sessionLabel(parentSession) : 'this session'}</strong>
              ) : (
                <label className="launcher-intent-control is-machine" title={`Computer: ${machineTitle}`}>
                  <MachineMark machine={machineTitle} size={16} />
                  <select value={machineId} onChange={(event) => chooseMachine(event.currentTarget.value)} aria-label="Computer">
                    {configuredMachines.map((machine) => (
                      <option key={machine.id} value={machine.id}>{serverDisplayName(machine, true)}</option>
                    ))}
                  </select>
                </label>
              )}
              <span>in</span>
              <button type="button" className="launcher-intent-control is-workspace" title={cwd || 'Choose a project folder'} onClick={() => setBrowserOpen((open) => !open)} aria-expanded={browserOpen}>
                <span className="workspace-folder-icon" aria-hidden />
                <strong>{workspaceTitle}</strong>
              </button>
            </h2>
            <p>{isDelegate ? 'Give it one focused job. It stays grouped with its parent.' : 'Describe the work below, or leave it blank to open an empty conversation.'}</p>
          </section>
          <div className="field launcher-task-field launcher-composer input-composer">
            <span className="sr-only">First request (optional)</span>
            <textarea
              className="input-textarea"
              autoFocus
              value={task}
              onChange={(event) => setTask(event.currentTarget.value)}
              onKeyDown={(event) => {
                if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
                event.preventDefault();
                event.currentTarget.form?.requestSubmit();
              }}
              placeholder={isDelegate ? 'Describe the work for this linked session…' : 'Ask an agent to work, or leave blank to open a conversation…'}
              rows={6}
            />
            <div className="launcher-composer-footer input-composer-footer">
              <div className="launcher-composer-context" aria-label="New session configuration">
                {isDelegate ? (
                  <span className="launcher-context-chip" title={`${serverDisplayName(getActiveServer(), true)} · ${cwd}`}><span className="launcher-machine-icon" aria-hidden />Inherited</span>
                ) : null}
                {tool !== 'shell' ? (
                  <>
                    <ModelPicker
                      provider={tool === 'claude-code' ? 'claude' : 'codex'}
                      value={selectedModel}
                      options={modelOptions}
                      loading={tool === 'codex' && codexModelsLoading}
                      error={tool === 'codex' ? codexModelsError : null}
                      onChange={selectModel}
                      defaultLabel={`${selectedTool.name} default`}
                      allowCustom
                      compact
                    />
                    <label className="launcher-effort-chip">
                      <span className="sr-only">Effort</span>
                      <select value={selectedEffort} onChange={(event) => selectEffort(event.currentTarget.value)} aria-label="Reasoning effort">
                        <option value="">Default effort</option>
                        {effortChoices.map((effort) => <option key={effort} value={effort}>{effortLabel(effort)}</option>)}
                      </select>
                    </label>
                    {tool === 'claude-code' ? (
                      <label className="launcher-permissions-chip">
                        <span className="sr-only">Permissions</span>
                        <select
                          value={claudeOptions.permissionMode ?? ''}
                          onChange={(event) => setClaudeOptions((current) => ({ ...current, permissionMode: event.currentTarget.value as ClaudeSessionOptions['permissionMode'] }))}
                          aria-label="Permissions"
                        >
                          <option value="">Settings permissions</option>
                          <option value="manual">Ask every time</option>
                          <option value="acceptEdits">Accept edits</option>
                          <option value="auto">Auto</option>
                          <option value="plan">Plan only</option>
                          <option value="dontAsk">Don’t ask</option>
                          <option value="bypassPermissions">Full access</option>
                        </select>
                      </label>
                    ) : (
                      <label className="launcher-permissions-chip">
                        <span className="sr-only">Permissions</span>
                        <select value={skipPerms ? 'full' : 'safe'} onChange={(event) => {
                          const fullAccess = event.currentTarget.value === 'full';
                          setSkipPerms(fullAccess);
                          if (!fullAccess && runtimeMode === 'rich') setRuntimeMode('terminal');
                        }} aria-label="Permissions">
                          <option value="safe">Ask when needed</option>
                          <option value="full">Full access</option>
                        </select>
                      </label>
                    )}
                  </>
                ) : null}
              </div>
              <div className="launcher-composer-actions">
                <button type={createdWithDeliveryError ? 'button' : 'submit'} className={`btn btn-primary launcher-composer-start${createdWithDeliveryError ? ' is-wide' : ''}`} disabled={!createdWithDeliveryError && (busy || !cwd.trim() || !profileValid)} onClick={createdWithDeliveryError ? onClose : undefined} aria-label={createdWithDeliveryError ? 'View session' : 'Start session'} title={createdWithDeliveryError ? 'View session' : 'Start session'}>
                  {createdWithDeliveryError ? 'View session' : busy ? '…' : '↑'}
                </button>
              </div>
            </div>
            <span className="launcher-send-hint">Enter sends · Shift+Enter adds a line</span>
            {requiresProviderLogin ? <span className="field-help">Finish the new account login first. Sessions will keep this request here instead of sending it into a login screen.</span> : null}
          </div>
          {isDelegate ? (
            <div className="launcher-inherited"><span>Runs with its parent</span><strong>{cwd}</strong><small>{serverDisplayName(getActiveServer(), true)} · grouped under {parentSession ? sessionLabel(parentSession) : 'the current session'}</small></div>
          ) : (
            <section className="launcher-workspace-shell">
              {browserOpen ? (
                <div className="launcher-workspace-picker">
                  {displayedWorkspaces.length > 0 ? (
                    <div className="recent-workspaces">
                      {displayedWorkspaces.map((item) => (
                        <button type="button" key={item.path} className={`${cwd === item.path ? 'is-active' : ''}${item.kind === 'somewhere' ? ' is-somewhere' : ''}`} onClick={() => { setCwd(item.path); setBrowserOpen(false); }}>
                          <span className="workspace-folder-icon" aria-hidden />
                          <span className="workspace-card-copy"><strong>{item.label}</strong><small>{openSessionWorkspacePaths.has(item.path) ? 'Open session folder' : workspaceKind(item.kind)}</small></span>
                          <span className="workspace-radio" aria-hidden />
                        </button>
                      ))}
                    </div>
                  ) : null}
                  <div className="launcher-directory-picker">
                    <DirectoryBrowser serverId={machineId} value={cwd} onChange={(path, confirmed) => { setCwd(path); if (confirmed) setBrowserOpen(false); }} />
                  </div>
                </div>
              ) : null}
              {cwd === homeWorkspace ? <span className="field-help">A project folder also avoids unrelated macOS prompts for protected folders such as Music and cloud drives.</span> : null}
            </section>
          )}
          <details className="launcher-advanced">
            <summary><strong>Advanced</strong><span>Account, tags, and provider settings</span></summary>
            <div className="launcher-advanced-body">
              {profileTool ? (
                <div className="field launcher-advanced-card account-profile-field">
                  <span className="field-label">Account</span>
                  <select
                    className="field-input"
                    value={profileChoice}
                    onChange={(event) => setProfileChoice(event.target.value)}
                    disabled={busy}
                  >
                    <option value="">Default</option>
                    {selectedProfile && profileChoice !== NEW_PROFILE && !toolProfiles.some((profile) => profile.name === selectedProfile) ? (
                      <option value={selectedProfile}>{selectedProfile} · inherited</option>
                    ) : null}
                    {toolProfiles.map((profile) => (
                      <option key={`${profile.tool}:${profile.name}`} value={profile.name}>{profile.name}</option>
                    ))}
                    <option value={NEW_PROFILE}>Add another login…</option>
                  </select>
                  {profileChoice === NEW_PROFILE ? (
                    <>
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
                      <span className="field-help">This opens a separate provider login and keeps its history separate.</span>
                    </>
                  ) : null}
                </div>
              ) : null}
              <details className="launcher-advanced-subsection">
                <summary>Tags <span>Optional organization</span></summary>
                <TagEditor value={tags} onChange={setTags} disabled={busy} />
              </details>
              {tool === 'claude-code' ? (
                <details className="launcher-advanced-subsection">
                  <summary>Claude options <span>Uses Settings by default</span></summary>
                  <div className="launcher-claude-options">
                    <label><span>Use Chrome</span><select value={claudeOptions.chrome ?? ''} onChange={(event) => setClaudeOptions((current) => ({ ...current, chrome: event.currentTarget.value as ClaudeSessionOptions['chrome'] }))}>
                      <option value="">Use Settings</option><option value="inherit">Claude default</option><option value="on">On</option><option value="off">Off</option>
                    </select></label>
                    <label><span>Somewhere tools</span><select value={claudeOptions.somewhereMcp ?? ''} onChange={(event) => setClaudeOptions((current) => ({ ...current, somewhereMcp: event.currentTarget.value as ClaudeSessionOptions['somewhereMcp'] }))}>
                      <option value="">Use Settings</option><option value="inherit">Use Claude configuration</option><option value="ensure">Make available</option>
                    </select></label>
                    <div className="launcher-claude-option-note"><strong>Remote Control</strong><small>Uses this machine’s consent choice from Settings. A session cannot turn it on by itself.</small></div>
                  </div>
                </details>
              ) : null}
              {tool === 'codex' ? (
                <div className="field launcher-advanced-card launcher-experience-field">
                  <span className="field-label">Experience</span>
                  <select className="field-input" value={runtimeMode} onChange={(event) => {
                    const nextMode = event.currentTarget.value as RuntimeMode;
                    setRuntimeMode(nextMode);
                    if (nextMode === 'rich') setSkipPerms(true);
                  }} aria-label="Session experience">
                    <option value="rich">Conversation</option>
                    <option value="terminal">Terminal</option>
                  </select>
                  <span className="field-help">Conversation is the normal Sessions view. Terminal opens Codex exactly as it appears in a shell.</span>
                </div>
              ) : tool === 'claude-code' ? (
                <div className="launcher-runtime-note"><strong>Conversation + Terminal</strong><span>One Claude session; switch views whenever you need the exact terminal.</span></div>
              ) : null}
              {tool === 'claude-code' ? (
                <details className="launcher-advanced-subsection launcher-troubleshooting">
                  <summary>Troubleshooting <span>Start without Claude customizations</span></summary>
                  <label className="field-checkbox">
                    <input type="checkbox" checked={claudeSafeMode} onChange={(event) => setClaudeSafeMode(event.currentTarget.checked)} />
                    <span className="field-checkbox-body">
                      <span>Temporarily turn customizations off</span>
                      <span className="field-hint">Disables hooks, plugins, MCP servers, skills, and project instructions for this session.</span>
                    </span>
                  </label>
                </details>
              ) : null}
            </div>
          </details>
          {error ? <div className="dialog-error">{error}</div> : null}
        </div>
      </form>
  );
  return embedded
    ? <section className="new-session-surface">{launcher}</section>
    : <div className="dialog-backdrop" onClick={onClose}>{launcher}</div>;
}
