// Real navigator, 100 records, fake daemon only. Disclosure never sends a write.
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { SessionNavigator } from '../src/components/SessionNavigator';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from '../tests/capability/fake-daemon';

const now = Date.now();
const names = ['Sessions', 'Website', 'Design tools', 'Documentation', 'Release checks'];
const sessions = names.flatMap((name, project) => Array.from({ length: 20 }, (_, index) => makeSession({
  id: `project-${project}-agent-${index}`, name: `${name} agent ${index + 1}`,
  cwd: `/work/${name}`, tool: index % 2 ? 'codex' : 'claude-code',
  lastUserMessageAt: now - index * 1000, createdAt: now - index * 1000,
  working: project === 0 && (index === 15 || index === 19),
  idleReason: project === 0 && index === 16 ? 'needs-input' : undefined,
  pinned: project === 0 && index === 17,
  tags: { project: name }
})));
const machines: FakeMachine[] = [{ id: 'fixture', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true, sessions,
  projects: names.map((name, index) => ({ id: `project-${index}`, name, implicit: false, roots: [`/work/${name}`],
    session_ids: sessions.filter((session) => session.cwd === `/work/${name}`).map((session) => session.id), live: 20, needs_input: index === 0 ? 1 : 0 })) }];
const daemon = installFakeDaemon(machines);
useFakeMachines(machines);
Object.assign(window, { projectGroupDaemon: daemon });
const theme = new URLSearchParams(location.search).get('theme') === 'light' ? 'light' : 'dark';
document.documentElement.dataset.theme = theme;
const noop = (): void => {};
createRoot(document.getElementById('root')!).render(
  <div className="app-shell operations-shell" data-theme={theme} style={{ height: '100dvh' }}>
    <div className="operations-frame">
      <SessionNavigator sessions={sessions} activeId="project-0-agent-18" machine="Fixture Mac"
        onOpen={noop} onOpenMachineSession={noop} onNew={noop} onAddProjectAgent={noop} onContinue={noop}
        onResumeSession={noop} onForkSession={async () => {}} onStartLinked={noop} openSessionIds={['project-0-agent-18']}
        onCloseView={noop} onReparent={async () => {}} />
      <section className="operations-content" aria-label="Conversation preview" />
    </div>
  </div>
);
