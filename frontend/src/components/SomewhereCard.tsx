import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  getSomewhereCLIStatus,
  isTauri,
  runNativeBackupAction,
  type BackupEnableResult,
  type BackupPushResult,
  type BackupStatus,
  type SomewhereCLIStatus
} from '../lib/tauriBridge';

const FALLBACK_INSTALL_COMMAND = 'npm install -g @somewhere-tech/cli';

function lastBackupLabel(value?: string): string {
  if (!value) return 'Not backed up yet';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : `Last backup ${date.toLocaleString()}`;
}

export function SomewhereCard(): JSX.Element {
  const [cli, setCLI] = useState<SomewhereCLIStatus | null>(null);
  const [backup, setBackup] = useState<BackupStatus | null>(null);
  const [project, setProject] = useState('');
  const [checking, setChecking] = useState(isTauri);
  const [busy, setBusy] = useState<'enable' | 'now' | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [recoveryPhrase, setRecoveryPhrase] = useState<string | null>(null);

  const refresh = useCallback(async (): Promise<void> => {
    if (!isTauri()) return;
    setChecking(true);
    setMessage(null);
    const [cliResult, backupResult] = await Promise.allSettled([
      getSomewhereCLIStatus(),
      runNativeBackupAction<BackupStatus>('status')
    ]);
    if (cliResult.status === 'fulfilled') setCLI(cliResult.value);
    else setMessage(cliResult.reason instanceof Error ? cliResult.reason.message : 'Could not inspect the Somewhere CLI');
    if (backupResult.status === 'fulfilled') {
      setBackup(backupResult.value.data);
      if (backupResult.value.data.project) setProject(backupResult.value.data.project);
    } else {
      setMessage((current) => current ?? (backupResult.reason instanceof Error ? backupResult.reason.message : 'Could not inspect backup status'));
    }
    setChecking(false);
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);

  const command = useMemo(() => {
    if (!cli?.installed) return cli?.installCommand ?? FALLBACK_INSTALL_COMMAND;
    return cli.updateAvailable ? cli.updateCommand : 'somewhere docs start';
  }, [cli]);

  const cliLabel = checking
    ? 'Checking CLI…'
    : !isTauri()
      ? 'Open in Sessions.app'
      : !cli?.installed
        ? 'CLI not installed'
        : cli.updateAvailable
          ? `${cli.installedVersion ?? 'CLI'} installed · ${cli.latestVersion ?? 'update'} available`
          : `CLI ${cli.installedVersion ?? ''} · ${cli.latestVersion ? 'up to date' : 'update check unavailable'}`;

  const copy = async (value: string, success: string): Promise<void> => {
    try {
      await navigator.clipboard.writeText(value);
      setMessage(success);
    } catch {
      setMessage(`Copy this value: ${value}`);
    }
  };

  const enableBackup = async (): Promise<void> => {
    if (busy || !project.trim()) return;
    setBusy('enable');
    setMessage(null);
    try {
      const result = await runNativeBackupAction<BackupEnableResult>('enable', project.trim());
      setBackup(result.data);
      setProject(result.data.project ?? project.trim());
      setRecoveryPhrase(result.data.recovery_phrase ?? null);
      setMessage(result.data.key_reused
        ? 'Encrypted backup is on. Your existing recovery key was reused.'
        : 'Encrypted backup is on. Save the recovery phrase before leaving this screen.');
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : 'Could not enable encrypted backup');
    } finally {
      setBusy(null);
    }
  };

  const backupNow = async (): Promise<void> => {
    if (busy) return;
    setBusy('now');
    setMessage(null);
    try {
      const pushed = await runNativeBackupAction<BackupPushResult>('now');
      const refreshed = await runNativeBackupAction<BackupStatus>('status');
      setBackup(refreshed.data);
      setMessage(`Backup complete: ${pushed.data.uploaded} uploaded, ${pushed.data.skipped} unchanged, ${pushed.data.unresolved} unresolved.`);
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : 'Could not back up sessions');
    } finally {
      setBusy(null);
    }
  };

  return (
    <section className="somewhere-card somewhere-hub">
      <header className="somewhere-hub-heading">
        <div>
          <a className="somewhere-brand-link" href="https://somewhere.tech" target="_blank" rel="noreferrer" aria-label="Open somewhere.tech">
            <img src={`${import.meta.env.BASE_URL}somewhere-logo.svg`} alt="" width="26" height="28" />
            <span>somewhere.tech</span>
          </a>
          <h2>Your Sessions cloud</h2>
          <p>Encrypted backup is available now. A dedicated Sessions account space, hosted search, cloud usage, and always-on machines are being designed here before the platform APIs ship.</p>
        </div>
        <span className="somewhere-live-badge">Backup available now</span>
      </header>

      <div className="somewhere-live-grid">
        <section className={`somewhere-backup${backup?.enabled ? ' is-enabled' : ''}`}>
          <header>
            <div><span>Encrypted backup</span><strong>{backup?.enabled ? 'On' : 'Not configured'}</strong></div>
            <span className="somewhere-status-dot" aria-hidden />
          </header>
          {backup?.enabled ? (
            <>
              <div className="somewhere-backup-project"><span>Somewhere project</span><code>{backup.project}</code></div>
              <div className="somewhere-backup-stats">
                <div><strong>{backup.last_session_count}</strong><span>sessions</span></div>
                <div><strong>{backup.last_push_count}</strong><span>uploaded</span></div>
                <div><strong>{backup.last_push_skipped}</strong><span>unchanged</span></div>
              </div>
              <p>{lastBackupLabel(backup.last_push_at)} · every {backup.interval || '15m'} · {backup.encrypt ? 'XChaCha20 encrypted locally' : 'plaintext backup'}</p>
              <div className="somewhere-backup-actions">
                <button type="button" className="btn" disabled={busy !== null} onClick={() => void backupNow()}>{busy === 'now' ? 'Backing up…' : 'Back up now'}</button>
                {!backup.encrypt ? <button type="button" className="btn btn-ghost" disabled={busy !== null} onClick={() => void enableBackup()}>{busy === 'enable' ? 'Encrypting…' : 'Turn on encryption'}</button> : null}
              </div>
            </>
          ) : (
            <>
              <p>Your Mac uploads directly to a Somewhere project using the existing <code>somewhere login</code> credential. Sessions stores only its path.</p>
              <label>
                Somewhere project
                <input value={project} onChange={(event) => setProject(event.currentTarget.value)} placeholder="my-sessions-backup" spellCheck={false} disabled={!isTauri() || busy !== null} />
              </label>
              <small>A dedicated Sessions slot on somewhere.tech is coming soon. For now, choose a project you own.</small>
              <button type="button" className="btn" disabled={!isTauri() || !cli?.installed || !project.trim() || busy !== null} onClick={() => void enableBackup()}>{busy === 'enable' ? 'Enabling…' : 'Enable encrypted backup'}</button>
            </>
          )}
        </section>

        <section className={`somewhere-cli${cli?.updateAvailable ? ' has-update' : ''}`}>
          <header><span>Somewhere CLI</span><strong>{cliLabel}</strong></header>
          <p>The CLI owns sign-in. Sessions reuses that local identity for direct backup without copying the token into an agent or workspace.</p>
          <code>{command}</code>
          <div className="somewhere-cli-actions">
            <button type="button" className="somewhere-copy" onClick={() => void copy(command, 'Command copied.')}>{!cli?.installed ? 'Copy install command' : cli.updateAvailable ? 'Copy update command' : 'Copy docs command'}</button>
            {isTauri() ? <button type="button" className="somewhere-copy" disabled={checking} onClick={() => void refresh()}>{checking ? 'Checking…' : 'Refresh'}</button> : null}
          </div>
          <small>{cli?.detail ?? 'Install the Somewhere CLI, then run somewhere login once.'}</small>
        </section>
      </div>

      {recoveryPhrase ? (
        <section className="somewhere-recovery" role="status">
          <div><span>Recovery phrase · shown here now</span><strong>Anyone with this phrase can decrypt your backup.</strong></div>
          <code>{recoveryPhrase}</code>
          <div>
            <button type="button" className="btn" onClick={() => void copy(recoveryPhrase, 'Recovery phrase copied. Store it somewhere safe.')}>Copy phrase</button>
            <button type="button" className="btn btn-ghost" onClick={() => setRecoveryPhrase(null)}>I saved it</button>
          </div>
        </section>
      ) : null}

      {message ? <div className="somewhere-message" role="status">{message}</div> : null}

      <div className="somewhere-coming-heading"><span>Account cloud</span><strong>Coming soon</strong></div>
      <div className="somewhere-coming-grid">
        <ComingSoon title="Session library" icon="◫">Browse encrypted archives by machine and recover Claude or Codex provider history.</ComingSoon>
        <ComingSoon title="Hosted search" icon="⌕">Search only the transcripts you explicitly choose to make server-indexable.</ComingSoon>
        <ComingSoon title="Cloud usage" icon="◒">Keep provider, token, cost, project, and daily rollups centrally without requiring transcript contents.</ComingSoon>
        <ComingSoon title="Password recovery" icon="◇">Wrap the local encryption key with a password-derived key; Somewhere never stores the password itself.</ComingSoon>
      </div>
    </section>
  );
}

function ComingSoon({ title, icon, children }: { title: string; icon: string; children: string }): JSX.Element {
  return (
    <article className="somewhere-coming-card">
      <span aria-hidden>{icon}</span>
      <div><strong>{title}</strong><p>{children}</p></div>
      <small>Coming soon</small>
    </article>
  );
}
