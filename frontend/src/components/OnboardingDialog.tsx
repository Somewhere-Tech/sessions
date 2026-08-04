import { useState } from 'react';
import { ProviderMark } from './ProviderBadge';

interface Props {
  machine: string;
  busy: boolean;
  error: string | null;
  onChoose: (remoteControl: 'enabled' | 'local-only', delegatedAccess: 'inherit' | 'autonomous') => Promise<void>;
}

export function OnboardingDialog({ machine, busy, error, onChoose }: Props): JSX.Element {
  const [step, setStep] = useState<1 | 2 | 3>(1);
  const [remoteControl, setRemoteControl] = useState<'enabled' | 'local-only'>('local-only');

  return (
    <div className="onboarding-gate" role="dialog" aria-modal="true" aria-labelledby="onboarding-title">
      <div className="onboarding-card">
        <header className="onboarding-brand">
          <span className="onboarding-brand-mark" aria-hidden>▦</span>
          <span><strong>Somewhere</strong><em>Sessions</em></span>
        </header>
        {step === 1 ? (
          <>
            <div className="onboarding-progress" aria-label="Step 1 of 3"><span className="is-active" /><span /><span /></div>
            <span className="onboarding-kicker">Welcome to Sessions</span>
            <h1 id="onboarding-title">Your agent work, organized and resumable.</h1>
            <p className="onboarding-lede">Sessions keeps Claude, Codex, and terminal work running in a local background service—even when you close this window.</p>
            <div className="onboarding-facts">
              <div><strong>Local by default</strong><span>Session history and runtime state stay on {machine}.</span></div>
              <div><strong>Built for people and agents</strong><span>The app and CLI share the same durable session truth.</span></div>
              <div><strong>You stay in control</strong><span>Nothing below changes sessions that are already running.</span></div>
            </div>
            <button type="button" className="btn btn-primary onboarding-next" onClick={() => setStep(2)}>Continue</button>
          </>
        ) : step === 2 ? (
          <>
            <div className="onboarding-progress" aria-label="Step 2 of 3"><span className="is-active" /><span className="is-active" /><span /></div>
            <div className="onboarding-provider"><ProviderMark provider="claude" size={38} /></div>
            <span className="onboarding-kicker">Claude Remote Control</span>
            <h1 id="onboarding-title">Use the same Claude session from anywhere?</h1>
            <p className="onboarding-lede">When enabled, new Claude sessions connect directly to Anthropic so they also appear on claude.ai and the Claude mobile app. They use your existing Claude subscription.</p>
            <div className="onboarding-privacy-note">
              <strong>What this changes</strong>
              <span>Claude makes an outbound connection to Anthropic. Sessions does not relay that conversation through Somewhere, and existing sessions are not restarted.</span>
            </div>
            {error ? <div className="onboarding-error" role="alert">{error}</div> : null}
            <div className="onboarding-actions">
              <button type="button" className="btn btn-primary" disabled={busy} onClick={() => { setRemoteControl('enabled'); setStep(3); }}>
                Enable Remote Control
              </button>
              <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => { setRemoteControl('local-only'); setStep(3); }}>
                Keep sessions local
              </button>
            </div>
            <button type="button" className="onboarding-back" disabled={busy} onClick={() => setStep(1)}>Back</button>
            <small className="onboarding-footnote">You can change this later in Settings. Agents and the Sessions CLI can inspect this choice, but cannot grant it for you.</small>
          </>
        ) : (
          <>
            <div className="onboarding-progress" aria-label="Step 3 of 3"><span className="is-active" /><span className="is-active" /><span className="is-active" /></div>
            <span className="onboarding-kicker">Delegated tasks</span>
            <h1 id="onboarding-title">How independently should child agents work?</h1>
            <p className="onboarding-lede">A manager session can start short-lived task workers. Sessions can give those workers the manager’s existing permissions, or let them work without approval prompts.</p>
            <div className="onboarding-privacy-note">
              <strong>No silent escalation</strong>
              <span>Inherited access is the safe default. Autonomous access is opt-in, applies only to agent-created children, and does not change sessions already running.</span>
            </div>
            {error ? <div className="onboarding-error" role="alert">{error}</div> : null}
            <div className="onboarding-actions">
              <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void onChoose(remoteControl, 'inherit')}>
                {busy ? 'Saving…' : 'Inherit manager permissions'}
              </button>
              <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => void onChoose(remoteControl, 'autonomous')}>
                Allow autonomous delegated work
              </button>
            </div>
            <button type="button" className="onboarding-back" disabled={busy} onClick={() => setStep(2)}>Back</button>
            <small className="onboarding-footnote">You can change this later in Settings. An agent cannot enable autonomous access for itself.</small>
          </>
        )}
      </div>
    </div>
  );
}
