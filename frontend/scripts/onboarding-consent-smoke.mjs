import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { readSessionsdSourceSync } from './lib/source-api.mjs';

const source = (path) => readFileSync(new URL(`../${path}`, import.meta.url), 'utf8');

const app = source('src/App.tsx');
const api = readSessionsdSourceSync();
const onboarding = source('src/components/OnboardingDialog.tsx');
const settings = source('src/components/SettingsView.tsx');
const settingsMenu = source('src/components/SettingsMenu.tsx');
const connections = source('src/components/ConnectionsView.tsx');
const newSession = source('src/components/NewSessionDialog.tsx');
const resume = source('src/components/ResumeDialog.tsx') + source('src/components/ResumeActions.tsx');

assert.match(app, /fetchOnboardingState/);
assert.match(app, /onboarding\.supported !== false && !onboarding\.complete/);
assert.match(app, /<OnboardingDialog/);
assert.match(app, /if \(!onboardingPending \|\| nativeClientOnly\) return;/);
assert.match(app, /nativeClientOnly && onboardingPending/);
assert.match(app, /Finish setup on \{machine\}:/);
assert.match(app, /Open Sessions on that computer once\./);
assert.match(app, /!nativeClientOnly && onboarding && onboarding\.supported !== false && !onboarding\.complete/);
assert.match(api, /X-Sessions-User-Consent': 'onboarding'/);
assert.match(api, /remoteControl: 'pending' \| 'enabled' \| 'local-only'/);
assert.match(api, /delegatedAccess: 'pending' \| 'inherit' \| 'autonomous'/);

assert.match(onboarding, /Enable Remote Control/);
assert.match(onboarding, /Keep sessions local/);
assert.match(onboarding, /connect directly to Anthropic/);
assert.match(onboarding, /Sessions does not relay that conversation through Somewhere/);
assert.match(onboarding, /cannot grant it for you/);
// Autonomous delegated work is the default and the primary choice; inheriting
// the manager's permissions is the explicit narrower option.
assert.match(onboarding, /onChoose\(remoteControl, 'autonomous'\)\}>\s*\{busy \? 'Saving…' : 'Let delegated work run on its own'\}/);
assert.match(onboarding, /Make them inherit my permissions/);
assert.match(onboarding, /an agent cannot widen this for itself/);
assert.doesNotMatch(onboarding, /onClick=\{onClose\}|launcher-close|dialog-head-link/);

assert.match(settings, /updateOnboardingPreference/);
assert.match(settings, /Keep sessions local/);
assert.match(settings, /Delegated agent access/);
assert.match(settings, /Approval questions stay open as Needs you/);
assert.match(settings, /Chosen on \{hostName\}/);
assert.match(settings, /disabled=\{props\.clientOnly \|\| props\.claudeBusy/);
assert.match(settings, /disabled=\{props\.clientOnly \|\| props\.delegationBusy/);
assert.match(settings, /if \(clientOnly \|\| providerBusy\) return;/);
assert.match(settingsMenu, /disabled=\{clientOnly \|\| aiBusy/);
assert.match(connections, /isTauri\(\) && !clientOnly/);
assert.match(connections, /disabled=\{clientOnly \|\| busy !== null\}/);
assert.match(connections, /<h2>Paired machines<\/h2>/);
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
