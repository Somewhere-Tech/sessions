import { lazy, Suspense, useState } from 'react';

const OnboardingSteps = lazy(() => import('./OnboardingSteps').then((module) => ({ default: module.OnboardingSteps })));

export interface OnboardingDialogProps {
  machine: string;
  busy: boolean;
  error: string | null;
  onAllowLocalNetwork: () => Promise<void>;
  onChoose: (remoteControl: 'enabled' | 'local-only', delegatedAccess: 'inherit' | 'autonomous') => Promise<void>;
}

export function OnboardingDialog(props: OnboardingDialogProps): JSX.Element {
  const [continued, setContinued] = useState(false);
  return (
    <div className="onboarding-gate" role="dialog" aria-modal="true" aria-labelledby="onboarding-title">
      <div className="onboarding-card">
        <header className="onboarding-brand">
          <span className="onboarding-brand-mark" aria-hidden>▦</span>
          <span><strong>Somewhere</strong><em>Sessions</em></span>
        </header>
        {continued ? (
          <Suspense fallback={<OnboardingProgress step={2} />}>
            <OnboardingSteps {...props} onBackToWelcome={() => setContinued(false)} />
          </Suspense>
        ) : (
          <>
            <OnboardingProgress step={1} />
            <span className="onboarding-kicker">Welcome to Sessions</span>
            <h1 id="onboarding-title">Your agent work, organized and resumable.</h1>
            <p className="onboarding-lede">Sessions keeps Claude, Codex, and terminal work running in a local background service—even when you close this window.</p>
            <div className="onboarding-facts">
              <div><strong>Local by default</strong><span>Session history and runtime state stay on {props.machine}.</span></div>
              <div><strong>Built for people and agents</strong><span>The app and CLI share the same durable session truth.</span></div>
              <div><strong>You stay in control</strong><span>Nothing below changes sessions that are already running.</span></div>
            </div>
            <button type="button" className="btn btn-primary onboarding-next" onClick={() => setContinued(true)}>Continue</button>
          </>
        )}
      </div>
    </div>
  );
}

export function OnboardingProgress({ step }: { step: number }): JSX.Element {
  return (
    <div className="onboarding-progress" aria-label={`Step ${step} of 5`}>
      {[1, 2, 3, 4, 5].map((position) => <span key={position} className={position <= step ? 'is-active' : undefined} />)}
    </div>
  );
}
