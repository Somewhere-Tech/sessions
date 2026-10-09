import { createRoot } from 'react-dom/client';
import { useState } from 'react';
import '../src/styles/globals.css';
import { SessionView } from '../src/components/SessionView';
import { RestartConversationHost } from '../src/components/RestartConversation';
import { SessionNavigator } from '../src/components/SessionNavigator';
import { NewSessionDialog } from '../src/components/NewSessionDialog';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from '../tests/capability/fake-daemon';

const session = makeSession({ id: 'conversation-preview', name: 'Make this Mac a better place to work',
  tool: 'codex', kind: 'codex-app-server', messageSubmit: true, working: true, model: 'gpt-6.1-sol', effort: 'high',
  profile: 'acct-413d791d8c697724c3a5c39e', cwd: '/Users/example/projects/sessions', tags: { project: 'Sessions' } });
const reviewer = makeSession({ id: 'reviewer-preview', name: 'Release reviewer', tool: 'claude-code',
  cwd: '/Users/example/projects/sessions', tags: { project: 'Sessions' } });
const writer = makeSession({ id: 'writer-preview', name: 'Write the launch announcement', tool: 'codex',
  kind: 'codex-app-server', cwd: '/Users/example/projects/website', lastUserMessageAt: Date.now() - 30_000 });
const machines: FakeMachine[] = [{ id: 'local', name: 'This Mac', host: 'localhost', port: 8787,
  isDefault: true, sessions: [session, reviewer, writer],
  directories: [{ path: session.cwd, label: 'Sessions', kind: 'project' }],
  projects: [{ id: 'sessions', name: 'Sessions', implicit: false, roots: [session.cwd], session_ids: [session.id, reviewer.id], live: 2, needs_input: 0 },
    { id: 'website', name: 'Launch site', implicit: false, roots: [writer.cwd], session_ids: [writer.id], live: 1, needs_input: 0 }],
  events: { [session.id]: [
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
// Optional failure journey for visual review; every request still goes to the
// fake daemon, never a running Sessions service or provider.
if (new URLSearchParams(location.search).has('delivery')) {
  const fakeFetch = window.fetch;
  let operationId = '';
  window.fetch = async (input, init) => {
    const path = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.origin).pathname;
    if (path.endsWith(`/sessions/${session.id}/submit`) && init?.method === 'POST') {
      operationId = JSON.parse(String(init.body)).operation_id as string;
      return Response.json({ operation_id: operationId, session_id: session.id, status: 'unknown', delivered: false, retry: false });
    }
    if (operationId && path.endsWith(`/message-deliveries/${operationId}`)) {
      return Response.json({ operation_id: operationId, session_id: session.id, status: 'accepted', delivered: true, retry: false });
    }
    return fakeFetch(input, init);
  };
}
const theme = new URLSearchParams(location.search).get('theme') === 'light' ? 'light' : 'dark';
document.documentElement.dataset.theme = theme;
function WorkspacePreview(): JSX.Element {
  const [activeId, setActiveId] = useState(session.id);
  const [launcher, setLauncher] = useState(new URLSearchParams(location.search).has('launcher'));
  const showProjects = new URLSearchParams(location.search).has('workspace');
  return <div className="app-shell operations-shell text-size-s" data-theme={theme} style={{ height: '100dvh' }}>
    <div className="operations-frame">
      {showProjects ? <SessionNavigator sessions={machines[0].sessions} activeId={activeId} machine="This Mac"
        onOpen={setActiveId} onOpenMachineSession={(_, id) => setActiveId(id)} onNew={() => setLauncher(true)}
        onContinue={() => {}} onResumeSession={() => {}} onForkSession={async () => {}} onStartLinked={() => {}}
        openSessionIds={[activeId]} onCloseView={() => {}} onReparent={async () => {}} /> : null}
      <section className="operations-content">
        <SessionView sessionId={activeId} isActive onCloseView={() => {}} onOpenSession={setActiveId} onFork={async () => {}} />
      </section>
    </div>
    {launcher ? <NewSessionDialog onClose={() => setLauncher(false)} onStarted={() => setLauncher(false)} /> : null}
    <RestartConversationHost />
  </div>;
}
createRoot(document.getElementById('root')!).render(<WorkspacePreview />);
