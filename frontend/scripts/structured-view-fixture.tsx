import React from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { RemoteView } from '../src/components/RemoteView';
import type { StructuredSessionEvent } from '../src/types';

const base = { source: 'codex-app-server', conversationId: 'thread-1' } as const;
const events: StructuredSessionEvent[] = [
  {
    ...base,
    type: 'user',
    uuid: 'user-1',
    timestamp: '2026-07-20T10:00:00Z',
    author: { kind: 'session', id: 'lane-1', name: 'PM Claude', client: 'sessions-cli' },
    message: { role: 'user', content: 'Build a first-class Codex GUI inside Sessions.' }
  },
  {
    ...base,
    type: 'codex', subtype: 'plan_updated', turnId: 'turn-1',
    explanation: 'Keep the durable Go runtime and make structured events the UI boundary.',
    plan: [
      { step: 'Preserve complete app-server events', status: 'completed' },
      { step: 'Render the conversation and activity', status: 'completed' },
      { step: 'Verify the signed desktop build', status: 'inProgress' }
    ]
  },
  {
    ...base,
    type: 'codex', subtype: 'item_completed', turnId: 'turn-1',
    timestamp: '2026-07-20T10:02:14Z',
    item: {
      id: 'reasoning-1', type: 'reasoning',
      summary: ['The current backend already owns the hard lifecycle boundaries, so the GUI should project those events instead of adding another server.']
    }
  },
  {
    ...base,
    type: 'codex', subtype: 'item_completed', turnId: 'turn-1',
    item: {
      id: 'command-1', type: 'commandExecution', status: 'completed',
      command: 'go test ./...', cwd: '/Users/example/sessions',
      aggregatedOutput: 'ok   github.com/somewhere-tech/sessions/runtime/internal/codexapp\n',
      exitCode: 0, durationMs: 881
    }
  },
  {
    ...base,
    type: 'codex', subtype: 'item_completed', turnId: 'turn-1',
    item: {
      id: 'files-1', type: 'fileChange', status: 'completed',
      changes: [{
        path: 'frontend/src/components/RemoteView.tsx', kind: 'update',
        diff: '@@ -1,2 +1,3 @@\n+ Conversation\n+ Activity timeline\n+ Plan progress'
      }]
    }
  },
  {
    ...base,
    type: 'codex', subtype: 'item_completed', turnId: 'turn-1',
    item: { id: 'update-1', type: 'agentMessage', phase: 'commentary', text: 'The structured event adapter is live. I am finishing the desktop verification now.' }
  },
  {
    ...base,
    type: 'codex', subtype: 'item_completed', turnId: 'turn-1',
    item: {
      id: 'answer-1', type: 'agentMessage', phase: 'final_answer',
      text: 'Sessions now has a real Codex conversation view with streaming activity, plans, command output, file diffs, reasoning summaries, context usage, and a safe interrupt control.'
    }
  },
  { ...base, type: 'codex', subtype: 'turn_completed', turnId: 'turn-1', status: 'completed' }
];

const sidebar = {
  parserName: 'Codex',
  parserIcon: '🟢',
  isWorking: false,
  timer: '',
  tokens: '4.8k',
  context: '18% of 258.0k',
  finalElapsed: '2m 14s',
  currentTask: '',
  checklist: []
};

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <main style={{ height: '100vh', display: 'flex' }}>
      <RemoteView
        sessionId="fixture-session"
        events={events}
        send={() => {}}
        connected
        hasEarlierClaudeEvents={false}
        loadingEarlierClaudeEvents={false}
        onLoadEarlierClaudeEvents={() => {}}
        sidebar={sidebar}
        cwd="/Users/example/sessions"
        onOpenTerminal={() => {}}
        provider="codex"
      />
    </main>
  </React.StrictMode>
);
