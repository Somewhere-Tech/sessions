import { useEffect, useRef, useState } from 'react';
import { fetchProfiles, generateConversationBriefing, type AccountProfile } from '../api/sessionsd';
import type { ForkPoint } from './ForkConfirmationDialog';
import type { SessionInfo } from '../types';

export function useCollaboratorContext(session: SessionInfo, destination: 'claude' | 'codex', serverId: string, point?: ForkPoint) {
  const [mode, setMode] = useState<'briefing' | 'conversation'>('briefing');
  const [briefing, setBriefing] = useState('');
  const [name, setName] = useState('');
  const [profile, setProfile] = useState('');
  const [profiles, setProfiles] = useState<AccountProfile[]>([]);
  const [profileError, setProfileError] = useState<string | null>(null);
  const [generating, setGenerating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const request = useRef<AbortController | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    void fetchProfiles(controller.signal, serverId).then(setProfiles).catch(() => {
      if (!controller.signal.aborted) setProfileError('Could not load accounts. Close and reopen to retry, or use the default account.');
    });
    return () => { controller.abort(); request.current?.abort(); };
  }, [serverId]);
  useEffect(() => {
    setProfile((session.tool === 'codex' ? 'codex' : 'claude') === destination ? session.profile ?? '' : '');
  }, [destination, session.profile, session.tool]);
  const generate = async (): Promise<void> => {
    if (request.current) return;
    const controller = new AbortController();
    request.current = controller;
    setGenerating(true); setError(null);
    let timedOut = false;
    const deadline = window.setTimeout(() => { timedOut = true; controller.abort(); }, 125_000);
    try { setBriefing((await generateConversationBriefing(session.id, point, serverId, controller.signal)).briefing); }
    catch (reason) {
      if (timedOut) setError('Briefing generation timed out. Your source is unchanged. Try again or write a briefing.');
      else if (!controller.signal.aborted) setError(reason instanceof Error ? reason.message : 'Could not generate a briefing. You can write one instead.');
    }
    finally { window.clearTimeout(deadline); request.current = null; setGenerating(false); }
  };
  return { mode, setMode, briefing, setBriefing, name, setName, profile, setProfile, profileError, profiles: profiles.filter((p) => p.tool === destination), generating, error, generate };
}

export function CollaboratorContext({ value, source, disabled }: {
  value: ReturnType<typeof useCollaboratorContext>; source: SessionInfo; disabled: boolean;
}): JSX.Element {
  return <div className="collaborator-context">
    <label>Agent name<input value={value.name} onChange={(event) => value.setName(event.target.value)} placeholder="e.g. Release reviewer" disabled={disabled} /></label>
    <fieldset disabled={disabled}><legend>Starting context</legend>
      <label><input type="radio" name="collaborator-context" checked={value.mode === 'briefing'} onChange={() => value.setMode('briefing')} /> Briefing <small>Start light; look up details when needed</small></label>
      <label><input type="radio" name="collaborator-context" checked={value.mode === 'conversation'} onChange={() => value.setMode('conversation')} /> Conversation copy <small>More history, more context</small></label>
    </fieldset>
    {value.mode === 'briefing' ? <>
      <label>Briefing<textarea value={value.briefing} onChange={(event) => value.setBriefing(event.target.value)} disabled={disabled || value.generating} rows={5} placeholder="Goal, decisions, restrictions, progress, and files to read…" /></label>
      <button type="button" className="btn btn-secondary" disabled={disabled || value.generating} onClick={() => void value.generate()}>{value.generating ? 'Preparing briefing…' : 'Generate draft briefing'}</button>
      <small>Drafted by {source.tool === 'codex' ? 'Codex' : 'Claude'} using the {source.profile || 'default'} account. Uses your allowance; the original stays unchanged. Review before starting.</small>
      <small>About {Math.ceil([...value.briefing].length / 4).toLocaleString()} tokens of briefing. Additional provider instructions may apply.</small>
      {value.error ? <p role="alert">{value.error}</p> : null}
    </> : null}
    <details><summary>Account</summary><label>Run this agent with<select aria-label="Destination account" value={value.profile} disabled={disabled} onChange={(event) => value.setProfile(event.target.value)}>
      <option value="">Machine default account</option>
      {value.profile && !value.profiles.some((p) => p.name === value.profile) ? <option value={value.profile}>{value.profile}</option> : null}
      {value.profiles.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
    </select></label>{value.profileError ? <p role="alert">{value.profileError}</p> : null}<small>Uses an existing account on this computer. Connect a new account through Add agent → Advanced first.</small></details>
  </div>;
}
