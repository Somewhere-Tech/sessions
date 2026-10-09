import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';
import { JSDOM } from 'jsdom';
import React, { act } from 'react';
import { createRoot } from 'react-dom/client';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-project-navigation-'));
try {
  const output = join(scratch, 'projects.mjs');
  await build({ entryPoints: ['src/lib/projectAgents.ts'], bundle: true, format: 'esm', platform: 'node', outfile: output, logLevel: 'silent' });
  const { groupAgents, recentAgents, resolvedProjects, projectMembershipKey, matchesResolvedProject, navigationProjectOptions } = await import(pathToFileURL(output));
  const session = (id, at, cwd) => ({ id, cwd, tool: 'codex', exited: false, createdAt: at, lastDataAt: at, name: id });
  // Deliberately collide daemon-local IDs and project IDs across computers.
  const a = session('same-session', 10, '/work/folder');
  const b = session('same-session', 30, '/else/folder');
  const c = session('older', 5, '/other');
  const snapshots = [{ server: { id: 'a' }, sessions: [a, c], error: null }, { server: { id: 'b' }, sessions: [b], error: null }];
  const projects = { a: [{ id: 'same-project', name: 'Design project', session_ids: [a.id] }], b: [{ id: 'same-project', name: 'Research project', session_ids: [b.id] }] };
  const membership = resolvedProjects(snapshots, projects);
  const pa = membership.get(projectMembershipKey('a', a.id));
  const pb = membership.get(projectMembershipKey('b', b.id));
  assert.notEqual(pa.id, pb.id, 'unrelated computers do not merge local project IDs');
  assert.equal(matchesResolvedProject(a, pa, 'all', 'DESIGN PROJECT'), true);
  assert.equal(matchesResolvedProject(b, pb, pa.id, ''), false);
  assert.equal(matchesResolvedProject(b, pb, pb.id, 'research'), true);
  const grouped = groupAgents(snapshots, projects, false, () => true);
  const recent = recentAgents(grouped);
  assert.equal(recent.length, 1, 'recent is one arrangement, not several project groups');
  assert.deepEqual(recent[0].rows.map((row) => row.server.id + ':' + row.session.id), ['b:same-session', 'a:same-session', 'a:older']);
  assert.deepEqual(recent[0].rows.slice(0, 2).map((row) => row.projectName), ['Research project', 'Design project']);
  const pinned = { ...grouped[0].rows[0], session: { ...c, pinned: true } };
  assert.equal(recentAgents([...grouped, { id: 'pinned', name: 'Pinned project', rows: [pinned] }])[0].rows[0].session.id, 'older', 'explicit pins retain priority in the recent arrangement');
  const duplicateNames = new Map([[projectMembershipKey('a', a.id), { id: pa.id, name: 'Same name' }], [projectMembershipKey('b', b.id), { id: pb.id, name: 'Same name' }]]);
  assert.deepEqual(navigationProjectOptions(duplicateNames, [{ id: 'a', name: 'This Mac' }, { id: 'b', name: 'Mini' }]).map((option) => option.name), ['Same name · Mini', 'Same name · This Mac']);
  const filtered = groupAgents(snapshots, projects, false, (s, serverId) => matchesResolvedProject(s, membership.get(projectMembershipKey(serverId, s.id)), pa.id, ''));
  assert.deepEqual(filtered.flatMap((group) => group.rows.map((row) => row.server.id)), ['a']);

  // Exercise the actual hook with controlled provider reads; no daemon/browser auth.
  const hookOutput = join(scratch, 'hook.mjs');
  await build({ entryPoints: ['src/hooks/useProjects.ts'], bundle: true, format: 'esm', platform: 'node', outfile: hookOutput, external: ['react'], logLevel: 'silent', plugins: [{
    name: 'controlled-project-read', setup(builder) {
      builder.onResolve({ filter: /api\/sessionsd$|lib\/servers$/ }, (args) => ({ path: args.path, namespace: 'fixture' }));
      builder.onLoad({ filter: /.*/, namespace: 'fixture' }, (args) => ({ contents: args.path.endsWith('servers')
        ? 'export const getServer = (id) => ({ id });'
        : 'export const fetchProjects = (_, server) => globalThis.projectRead(server.id);', loader: 'js' }));
      builder.onResolve({ filter: /^react$/ }, () => ({ path: pathToFileURL(join(process.cwd(), 'node_modules/react/index.js')).href, external: true }));
    }
  }] });
  const dom = new JSDOM('<div id="root"></div>');
  const old = { window: globalThis.window, document: globalThis.document, flag: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  let poll, failed = false, latest;
  window.setInterval = (callback) => { poll = callback; return 1; };
  window.clearInterval = () => {};
  globalThis.projectRead = async (serverId) => { if (failed) throw new Error('Computer did not answer'); return projects[serverId] ?? []; };
  const { useProjects } = await import(pathToFileURL(hookOutput));
  function Fixture({ serverId }) { latest = useProjects(['same-session'], true, serverId); return null; }
  const root = createRoot(document.getElementById('root'));
  try {
    await act(async () => root.render(React.createElement(Fixture, { serverId: 'a' })));
    assert.equal(latest.projects[0].name, 'Design project');
    failed = true;
    await act(async () => { poll(); });
    assert.equal(latest.projects[0].name, 'Design project', 'refresh failure retains last-known membership');
    assert.equal(latest.error, 'Computer did not answer');
    await act(async () => root.render(React.createElement(Fixture, { serverId: 'b' })));
    assert.equal(latest.projects.length, 0, 'a failed different machine never inherits the old names');
    failed = false;
    await act(async () => { poll(); });
    assert.equal(latest.projects[0].name, 'Research project');
    assert.equal(latest.error, null, 'successful refresh clears stale notice');
  } finally {
    await act(async () => root.unmount());
    dom.window.close();
    globalThis.window = old.window; globalThis.document = old.document; globalThis.IS_REACT_ACT_ENVIRONMENT = old.flag;
    delete globalThis.projectRead;
  }
  console.log('project-navigation smoke: resolved fleet identity, recent ordering and stale recovery passed');
} finally {
  await rm(scratch, { recursive: true, force: true });
}
