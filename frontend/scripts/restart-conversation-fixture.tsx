import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { RestartConversation, RestartConversationHost } from '../src/components/RestartConversation';
import { useDurableDraft } from '../src/hooks/useDurableDraft';
import { useServers } from '../src/lib/servers';
import { useSessions } from '../src/store/sessions';
import type { SessionInfo } from '../src/types';
import '../src/styles/globals.css';

const source = {
  id: 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee', name: 'Restart fixture', tool: 'claude-code', kind: 'claude-structured',
  args: [], model: 'fixture-model', profile: 'fixture-work', permissions: 'constrained', working: true, pid: 4242,
  cwd: '/fixture', createdAt: Date.now(), exited: false
} as SessionInfo;
useServers.setState({ servers: [{ id: 'fixture', name: 'Fixture', host: '127.0.0.1', port: Number(window.location.port), isDefault: true }], activeId: 'fixture' });
useSessions.setState({ serverId: 'fixture', sessions: [source], activeId: source.id, hydrated: true });

function Draft({ id }: { id: string }): JSX.Element {
  const draft = useDurableDraft('fixture', id);
  return <label>Unsent draft<textarea aria-label="Unsent draft" value={draft.text} onChange={(event) => draft.setText(event.target.value)} /></label>;
}
function Fixture(): JSX.Element {
  const [showSource, setShowSource] = useState(true);
  const [opened, setOpened] = useState('');
  return <main style={{ padding: 24 }}>
    {showSource ? <><Draft id={source.id} /><RestartConversation session={source} onOpen={(id) => { setOpened(id); setShowSource(false); }} /></> : null}
    <button data-hide-source onClick={() => setShowSource(false)}>Simulate source row retirement</button>
    {opened ? <><output data-opened>{opened}</output><Draft id={opened} /></> : null}
    <RestartConversationHost />
  </main>;
}
createRoot(document.getElementById('root')!).render(<Fixture />);
