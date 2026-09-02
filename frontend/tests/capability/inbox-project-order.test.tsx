import { describe, expect, it } from 'vitest';
import { buildInboxLayout, type ProjectRef } from '../../src/lib/inboxSections';
import { makeSession } from './fake-daemon';

describe('capability: order inbox projects by the person\'s pins', () => {
  it('places pinned projects before more recently active unpinned projects', () => {
    const olderPinned = makeSession({ id: 'older-pinned', lastDataAt: 100 });
    const newer = makeSession({ id: 'newer', lastDataAt: 200 });
    const projects = new Map<string, ProjectRef>([
      [olderPinned.id, { id: 'pinned', name: 'Pinned project', implicit: false, pinned: true }],
      [newer.id, { id: 'recent', name: 'Recent project', implicit: false, pinned: false }]
    ]);

    const layout = buildInboxLayout({
      live: [newer, olderPinned],
      ended: [],
      lastActivity: (session) => session.lastDataAt,
      projectFor: (session) => projects.get(session.id) ?? null
    });

    expect(layout.sections.map((section) => section.name)).toEqual(['Pinned project', 'Recent project']);
  });
});
