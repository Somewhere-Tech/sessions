import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { SessionView } from '../src/components/SessionView';
import { RestartConversationHost } from '../src/components/RestartConversation';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from '../tests/capability/fake-daemon';

const session = makeSession({ id: 'conversation-preview', name: 'Make this Mac a better place to work',
  tool: 'codex', kind: 'codex-app-server', messageSubmit: true, working: true, model: 'gpt-6.1-sol', effort: 'high',
  profile: 'acct-413d791d8c697724c3a5c39e', cwd: '/Users/example/projects/sessions' });
const machines: FakeMachine[] = [{ id: 'local', name: 'This Mac', host: 'localhost', port: 8787,
  isDefault: true, sessions: [session], events: { [session.id]: [
    { timestamp: '2026-09-30T16:35:00Z', source: 'codex-app-server', type: 'user', message: { role: 'user', content: 'Can you look at this computer and see what would make it better for coding and creative work?' } },
    { timestamp: '2026-09-30T16:35:05Z', source: 'codex-app-server', type: 'codex', turnId: 'review', subtype: 'turn_started' },
    { timestamp: '2026-09-30T16:36:00Z', source: 'codex-app-server', type: 'codex', turnId: 'review', subtype: 'item_completed', item: {
      id: 'before', type: 'agentMessage', phase: 'commentary', text: 'I’ll check storage and the development setup. I’ll keep this read-only.' } },
    { timestamp: '2026-09-30T16:37:00Z', source: 'codex-app-server', type: 'user', turnId: 'review', subtype: 'user_steer', uuid: 'followup',
      message: { role: 'user', content: 'Please check the external drive too.' } },
    { timestamp: '2026-09-30T16:39:00Z', source: 'codex-app-server', type: 'codex', turnId: 'review', subtype: 'item_completed', item: {
      id: 'after', type: 'agentMessage', phase: 'commentary', text: 'I’ll include the external drive in the review and won’t change its format or move any files.' } }
  ] } }];
const daemon = installFakeDaemon(machines);
useFakeMachines(machines);
Object.assign(window, { headerDaemon: daemon });
const theme = new URLSearchParams(location.search).get('theme') === 'light' ? 'light' : 'dark';
document.documentElement.dataset.theme = theme;
createRoot(document.getElementById('root')!).render(
  <div className="app-shell operations-shell text-size-s" data-theme={theme} style={{ height: '100dvh' }}>
    <SessionView sessionId={session.id} isActive onCloseView={() => {}} onOpenSession={() => {}} onFork={async () => {}} />
    <RestartConversationHost />
  </div>
);
