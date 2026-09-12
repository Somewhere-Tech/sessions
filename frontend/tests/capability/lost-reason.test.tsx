// CAPABILITY: a lost session says why it is lost, and what to do about it.
//
// The MacBook rebooted at 10:15 on 11 September. The daemon retired seven
// runners and the app showed seven sessions as "lost" with nothing else — no
// reason, no time, and no sign that resuming was the one thing to do. The
// daemon knew: a runner whose process began before the machine started did not
// crash and was not ended.
import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Workbench } from './harness';
import { classifySession, lostSessionNote } from '../../src/lib/sessionStatus';
import { notConnectedReason } from '../../src/lib/inboxSections';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';
import type { SessionInfo } from '../../src/types';

function lostSession(reason: string, extra: Partial<SessionInfo> = {}): SessionInfo {
  return Object.assign(makeSession({ id: `lost-${reason.replace(/\s/g, '-')}`, name: 'Release prep' }), {
    unreachable: true,
    unreachableReason: 'runner-lost',
    runnerGone: true,
    lostReason: reason,
    lostAt: Date.now() - 45 * 60_000,
    ...extra
  }) as SessionInfo;
}

describe('capability: a lost session says why', () => {
  it('names the reboot, the exit, and the silence differently', () => {
    expect(lostSessionNote(lostSession('machine rebooted'))).toMatch(/this machine restarted/);
    expect(lostSessionNote(lostSession('runner exited'))).toMatch(/process ended/);
    expect(lostSessionNote(lostSession('daemon lost contact'))).toMatch(/lost contact/);
    // Each one ends with the single action that applies.
    for (const reason of ['machine rebooted', 'runner exited', 'daemon lost contact']) {
      expect(lostSessionNote(lostSession(reason))).toMatch(/Resume to continue$/);
    }
    // A session that is not lost has nothing to say about it.
    expect(lostSessionNote(makeSession({ id: 'fine' }))).toBe('');
  });

  it('never reads as idle', () => {
    const status = classifySession(lostSession('machine rebooted'));
    expect(status.state).not.toBe('ready');
    expect(status.state).not.toBe('finished');
    expect(status.label).not.toMatch(/idle/i);
    expect(status.wantsAttention).toBe(true);
  });

  // The fold above the not-connected group used to say "runner is gone" for
  // every one of them, which is true and useless.
  it('replaces the generic not-connected line with the real reason', () => {
    expect(notConnectedReason(lostSession('machine rebooted'))).toMatch(/machine restarted/);
    const unknown = Object.assign(makeSession({ id: 'no-reason' }), {
      unreachable: true, unreachableReason: 'runner-lost', runnerGone: true
    }) as SessionInfo;
    expect(notConnectedReason(unknown)).toBe('runner is gone · provider conversation is saved');
  });
});

describe('capability: the row carries it', () => {
  it('shows the reason on the navigator row', async () => {
    const session = lostSession('machine rebooted');
    const machine: FakeMachine = {
      id: 'local', name: 'This Mac', host: 'localhost', port: 8787, isDefault: true, sessions: [session]
    };
    window.localStorage.setItem('sessions:projects-machine-scope', 'local');
    installFakeDaemon([machine]);
    useFakeMachines([machine], 'local');

    render(<Workbench />);

    // A lost session is folded away from the ones the person can type into,
    // and the fold itself says why rather than "runner is gone".
    const fold = await screen.findByRole('button', { name: /Not connected . 1/ });
    expect(fold.textContent).toMatch(/this machine restarted/);

    await userEvent.setup().click(fold);
    const row = (await screen.findByText('Release prep')).closest('.session-nav-row') as HTMLElement;
    expect(within(row).getByText(/this machine restarted/)).toBeInTheDocument();
    expect(within(row).getByText(/Resume to continue/)).toBeInTheDocument();
  }, 20_000);
});
