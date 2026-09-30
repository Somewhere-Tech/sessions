import { useState } from 'react';
import { requestFleetMagicLink, verifyFleetMagicLink } from '../api/sessionsd';
import { ProviderMark } from './ProviderBadge';
import { OnboardingProgress, type OnboardingDialogProps } from './OnboardingDialog';

export function OnboardingSteps({ busy, error, onAllowLocalNetwork, onChoose, onBackToWelcome }: OnboardingDialogProps & {
  onBackToWelcome: () => void;
}): JSX.Element {
  const [step, setStep] = useState<2 | 3 | 4 | 5>(2);
  const [remoteControl, setRemoteControl] = useState<'enabled' | 'local-only'>('local-only');
  const [localNetworkBusy, setLocalNetworkBusy] = useState(false);

  const allowLocalNetwork = async (): Promise<void> => {
    setLocalNetworkBusy(true);
    await onAllowLocalNetwork().catch(() => undefined);
    setLocalNetworkBusy(false);
    setStep(4);
  };

  if (step === 2) {
    return (
      <>
        <OnboardingProgress step={2} />
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
          <button type="button" className="btn btn-primary" disabled={busy} onClick={() => { setRemoteControl('enabled'); setStep(3); }}>Enable Remote Control</button>
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => { setRemoteControl('local-only'); setStep(3); }}>Keep sessions local</button>
        </div>
        <button type="button" className="onboarding-back" disabled={busy} onClick={onBackToWelcome}>Back</button>
        <small className="onboarding-footnote">You can change this later in Settings. Agents and the Sessions CLI can inspect this choice, but cannot grant it for you.</small>
      </>
    );
  }
  if (step === 3) {
    return (
      <>
        <OnboardingProgress step={3} />
        <span className="onboarding-kicker">Fleet</span>
        <h1 id="onboarding-title">Find your other Sessions machines?</h1>
        <p className="onboarding-lede">Find Sessions Macs on this trusted network. macOS asks once before sessionsd can connect.</p>
        <div className="onboarding-privacy-note"><strong>What this allows</strong><span>sessionsd connects; agents need no permission.</span></div>
        <div className="onboarding-actions">
          <button type="button" className="btn btn-primary" disabled={localNetworkBusy} onClick={() => void allowLocalNetwork()}>{localNetworkBusy ? 'Waiting for macOS…' : 'Allow local network'}</button>
          <button type="button" className="btn btn-ghost" disabled={localNetworkBusy} onClick={() => setStep(4)}>Not now</button>
        </div>
        <button type="button" className="onboarding-back" disabled={localNetworkBusy} onClick={() => setStep(2)}>Back</button>
        <small className="onboarding-footnote">Retry in Settings › Fleet. Tailscale is exempt.</small>
      </>
    );
  }
  if (step === 4) return <SomewhereOnboardingStep onBack={() => setStep(3)} onContinue={() => setStep(5)} />;
  return (
    <>
      <OnboardingProgress step={5} />
      <span className="onboarding-kicker">Delegated tasks</span>
      <h1 id="onboarding-title">How independently should child agents work?</h1>
      <p className="onboarding-lede">A manager session can start short-lived task workers. Sessions can give those workers the manager’s existing permissions, or let them work without approval prompts.</p>
      <div className="onboarding-privacy-note"><strong>Autonomous by default</strong><span>Agent-created children may run commands without asking. This does not change sessions already running.</span></div>
      {error ? <div className="onboarding-error" role="alert">{error}</div> : null}
      <div className="onboarding-actions">
        <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void onChoose(remoteControl, 'autonomous')}>{busy ? 'Saving…' : 'Default — let delegated work run on its own'}</button>
        <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => void onChoose(remoteControl, 'inherit')}>Make them inherit my permissions</button>
      </div>
      <button type="button" className="onboarding-back" disabled={busy} onClick={() => setStep(4)}>Back</button>
      <small className="onboarding-footnote">Inherited access is the safer alternative. You can change this later in Settings; an agent cannot widen this for itself.</small>
    </>
  );
}

function SomewhereOnboardingStep({ onBack, onContinue }: { onBack: () => void; onContinue: () => void }): JSX.Element {
  const [email, setEmail] = useState('');
  const [code, setCode] = useState('');
  const [sent, setSent] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const submit = async (): Promise<void> => {
    setBusy(true); setMessage(null); setFailed(false);
    try {
      await requestFleetMagicLink(email);
      setSent(true); setMessage(`Somewhere sent a single-use code or link to ${email.trim()}.`);
    } catch (reason) {
      setFailed(true); setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally { setBusy(false); }
  };
  const verify = async (): Promise<void> => {
    setBusy(true); setMessage(null); setFailed(false);
    try {
      await verifyFleetMagicLink(code);
      onContinue();
    } catch (reason) {
      setFailed(true); setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally { setBusy(false); }
  };
  return (
    <>
      <OnboardingProgress step={4} />
      <span className="onboarding-kicker">Optional fleet account</span>
      <h1 id="onboarding-title">Sign in to Somewhere?</h1>
      <p className="onboarding-lede">A Somewhere account registers this machine so your fleet can appear on every signed-in device. Sessions still works without one.</p>
      <div className="onboarding-privacy-note"><strong>Directory metadata only</strong><span>Somewhere sees this machine’s name, reachable endpoints, public key, and Sessions version—never session content or provider credentials.</span></div>
      <div className="onboarding-account-fields">
        <input type="email" value={email} disabled={busy || sent} placeholder="you@example.com" autoComplete="email" onChange={(event) => setEmail(event.currentTarget.value)} />
        {sent ? <input value={code} disabled={busy} placeholder="Six-digit code or link token" autoComplete="one-time-code" onChange={(event) => setCode(event.currentTarget.value)} onKeyDown={(event) => { if (event.key === 'Enter' && code.trim()) void verify(); }} /> : null}
      </div>
      {message ? <div className={failed ? 'onboarding-error' : 'onboarding-status'} role={failed ? 'alert' : 'status'}>{message}</div> : null}
      <div className="onboarding-actions">
        <button type="button" className="btn btn-primary" disabled={busy || (sent ? !code.trim() : !email.trim())} onClick={() => void (sent ? verify() : submit())}>{busy ? 'Waiting…' : sent ? 'Verify code' : 'Sign in to Somewhere'}</button>
        <button type="button" className="btn btn-ghost" disabled={busy} onClick={onContinue}>Skip — Sessions works on this network without it</button>
      </div>
      <button type="button" className="onboarding-back" disabled={busy} onClick={onBack}>Back</button>
    </>
  );
}
