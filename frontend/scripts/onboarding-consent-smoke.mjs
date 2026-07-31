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
assert.match(api, /X-Sessions-User-Consent': 'remote-control'/);
assert.match(api, /remoteControl: 'pending' \| 'enabled' \| 'local-only'/);

assert.match(onboarding, /Enable Remote Control/);
assert.match(onboarding, /Keep sessions local/);
assert.match(onboarding, /connect directly to Anthropic/);
assert.match(onboarding, /Sessions does not relay that conversation through Somewhere/);
assert.match(onboarding, /cannot grant it for you/);
assert.doesNotMatch(onboarding, /onClick=\{onClose\}|launcher-close|dialog-head-link/);

assert.match(settings, /updateOnboardingPreference/);
assert.match(settings, /Keep sessions local/);
assert.doesNotMatch(settings, /<option value="inherit">Use Claude’s setting<\/option>/);
assert.doesNotMatch(newSession, /<span>Remote Control<\/span><select/);
assert.match(newSession, /A session cannot turn it on by itself/);
assert.match(resume, /Remote Control follows the explicit choice for the destination machine in Settings/);
assert.doesNotMatch(resume, /className="resume-remote-control"/);

for (const file of [app, api, onboarding, settings, newSession, resume]) {
  assert.doesNotMatch(file, /localStorage.*remote.?control/i);
}

console.log('onboarding consent smoke: ok');
