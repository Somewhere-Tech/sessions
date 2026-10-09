// The real App with one manager and two delegated lanes, fake daemon only.
// The reviewer's first request is unconfirmed, so its lane card carries the
// partial-delivery next action the smoke reads at every width.
import React from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { App } from '../src/App';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from '../tests/capability/fake-daemon';

const now = Date.now();
const lead = makeSession({
  id: 'lead', name: 'Release lead', tool: 'claude-code', cmd: 'claude', cwd: '/work/sessions',
  branch: 'main', creatorKind: 'user', creatorId: 'uid:501', lastUserMessageAt: now - 20_000, createdAt: now - 600_000
});
const reviewer = makeSession({
  id: 'reviewer', name: 'Review the narrow-screen lanes fix', tool: 'claude-code', cmd: 'claude', kind: 'claude-structured',
  cwd: '/work/sessions-wt/lanes-fix', worktreePath: '/work/sessions-wt/lanes-fix', branch: 'codex/lanes-fix',
  creatorKind: 'session', creatorId: 'lead', parentSessionId: 'lead', delegationKind: 'agent', creatorAncestry: ['lead'],
  idleReason: 'never-started', createdAt: now - 30_000, messageSubmit: true, runnerProtocol: 5,
  start: {
    phase: 'prompt-unknown',
    prompt: { status: 'unknown', retry: false, reason: 'the runner did not acknowledge the first request before the connection closed' },
    evidence: 'Sessions cannot tell whether the first request reached the runner',
    evidence_source: 'operation-receipt',
    recovery: { action: 'inspect', detail: 'Read the conversation before sending again. Sessions did not resend anything.' }
  }
});
const worker = makeSession({
  id: 'worker', name: 'Fix the lanes panel', tool: 'codex', cmd: 'codex', kind: 'codex-app-server',
  cwd: '/work/sessions-wt/lanes-fix', worktreePath: '/work/sessions-wt/lanes-fix', branch: 'codex/lanes-fix',
  creatorKind: 'session', creatorId: 'lead', parentSessionId: 'lead', delegationKind: 'agent', creatorAncestry: ['lead'],
  idleReason: 'completed', lastAgentMessageAt: now - 60_000, lastUserMessageAt: now - 300_000, createdAt: now - 400_000,
  messageSubmit: true, runnerProtocol: 5
});
const machine: FakeMachine = {
  id: 'fixture', name: 'Fixture Mac', host: '127.0.0.1', port: 8787, isDefault: true,
  sessions: [lead, reviewer, worker],
  projects: [{
    id: 'p-sessions', name: 'Sessions', implicit: false, roots: ['/work/sessions', '/work/sessions-wt'],
    session_ids: ['lead', 'reviewer', 'worker'], live: 3, needs_input: 0
  }]
};
const daemon = installFakeDaemon([machine]);
useFakeMachines([machine]);
Object.assign(window, { __subagentsPanelFixture: { daemon } });

createRoot(document.getElementById('root')!).render(<React.StrictMode><App /></React.StrictMode>);
