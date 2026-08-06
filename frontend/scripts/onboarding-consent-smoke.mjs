import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const source = (path) => readFileSync(new URL(`../${path}`, import.meta.url), 'utf8');

const app = source('src/App.tsx');
const api = source('src/api/sessionsd.ts');
const onboarding = source('src/components/OnboardingDialog.tsx');
const settings = source('src/components/SettingsView.tsx');
const newSession = source('src/components/NewSessionDialog.tsx');
const resume = source('src/components/ResumeDialog.tsx');

assert.match(app, /fetchOnboardingState/);
assert.match(app, /onboarding\.supported !== false && !onboarding\.complete/);
assert.match(app, /<OnboardingDialog/);
assert.match(api, /X-Sessions-User-Consent': 'onboarding'/);
assert.match(api, /remoteControl: 'pending' \| 'enabled' \| 'local-only'/);
assert.match(api, /delegatedAccess: 'pending' \| 'inherit' \| 'autonomous'/);

assert.match(onboarding, /Enable Remote Control/);
assert.match(onboarding, /Keep sessions local/);
assert.match(onboarding, /connect directly to Anthropic/);
assert.match(onboarding, /Sessions does not relay that conversation through Somewhere/);
assert.match(onboarding, /cannot grant it for you/);
assert.match(onboarding, /Inherit manager permissions/);
assert.match(onboarding, /Allow autonomous delegated work/);
assert.match(onboarding, /An agent cannot enable autonomous access for itself/);
assert.doesNotMatch(onboarding, /onClick=\{onClose\}|launcher-close|dialog-head-link/);

assert.match(settings, /updateOnboardingPreference/);
assert.match(settings, /Keep sessions local/);
assert.match(settings, /Delegated task access/);
assert.match(settings, /Approval questions stay open as Needs you and are never accepted automatically/);
assert.doesNotMatch(settings, /<option value="inherit">Use Claude’s setting<\/option>/);
assert.doesNotMatch(newSession, /<span>Remote Control<\/span><select/);
assert.match(newSession, /A session cannot turn it on by itself/);
assert.match(resume, /Remote Control follows the explicit choice for the destination machine in Settings/);
assert.doesNotMatch(resume, /className="resume-remote-control"/);

// "Continue in Terminal with Remote Control" ends a live Rich runtime, which
// is irreversible. Remote Control itself is a machine-level consent boundary,
// so that request can only be honored when this machine already opted in. The
// consent read must therefore happen BEFORE endSession — otherwise the user
// spends the session and still does not get Remote Control.
const sessionView = source('src/components/SessionView.tsx');
const continueInTerminal = sessionView.slice(
  sessionView.indexOf('const continueInTerminal'),
  sessionView.indexOf('const forkFromVisibleMessage')
);
assert.ok(continueInTerminal, 'continueInTerminal must exist in SessionView');
assert.match(continueInTerminal, /fetchOnboardingState/);
assert.ok(
  continueInTerminal.indexOf('fetchOnboardingState') < continueInTerminal.indexOf('await endSession'),
  'the Remote Control consent check must run before the session is ended'
);
assert.match(continueInTerminal, /this session is still running/);
// The resumed session inherits the machine's Settings choice. Passing a
// per-resume Remote Control argument would be silently ignored — ResumeDialog
// deliberately has no such input — so it must not be passed at all.
assert.match(continueInTerminal, /onResume\(session, 'claude', 'terminal'\);/);
assert.doesNotMatch(continueInTerminal, /onResume\([^)]*enableRemoteControl/);

for (const file of [app, api, onboarding, settings, newSession, resume]) {
  assert.doesNotMatch(file, /localStorage.*remote.?control/i);
}

console.log('onboarding consent smoke: ok');
