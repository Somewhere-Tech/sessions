// CAPABILITY: a "needs login" claim shows the provider's own words, and steps
// out of the way when the person can already see the terminal.
//
// The founder's session on 11 September was logged in and working while the
// header showed NEEDS LOGIN and a card said "Claude is not logged in — Open the
// terminal to log in". The claim had been read out of the agent's own grep
// output. The daemon no longer raises a fault from words in tool output; what
// this file covers is the other half — when Sessions does make the claim, the
// person can see what it rests on, and the card does not offer to open a
// terminal that is already on screen.
import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { ProviderFaultCard } from '../../src/components/ProviderFaultCard';
import { classifySession } from '../../src/lib/sessionStatus';
import { makeSession } from './fake-daemon';

const EVIDENCE = '⏺ API Error: 401 {"type":"error","error":{"type":"authentication_error"}}';

describe('capability: the card shows the evidence', () => {
  it('prints the provider line the claim rests on', () => {
    render(
      <ProviderFaultCard
        sessionId="s" failureKind="auth" detail="Claude is not logged in"
        evidence={EVIDENCE} rich={false} onOpenTerminal={() => {}}
      />
    );

    expect(screen.getByText('Claude is not logged in')).toBeInTheDocument();
    expect(screen.getByText(EVIDENCE)).toBeInTheDocument();
    expect(screen.getByText(EVIDENCE).title).toMatch(/The provider printed this/);
  });

  // A daemon too old to send the evidence still renders a usable card; it must
  // not print an empty quotation implying the provider said nothing.
  it('says no more than it knows when no evidence came back', () => {
    const { container } = render(
      <ProviderFaultCard
        sessionId="s" failureKind="auth" detail="Claude is not logged in"
        rich={false} onOpenTerminal={() => {}}
      />
    );

    expect(container.querySelector('.provider-fault-evidence')).toBeNull();
    expect(screen.getByText('Claude is not logged in')).toBeInTheDocument();
  });
});

describe('capability: the card gets out of the way of the terminal', () => {
  it('is the full card with an Open Terminal action when the terminal is closed', () => {
    render(
      <ProviderFaultCard
        sessionId="s" failureKind="auth" detail="Claude is not logged in"
        evidence={EVIDENCE} rich={false} onOpenTerminal={() => {}}
      />
    );

    const card = screen.getByRole('group', { name: 'Provider trouble' });
    expect(within(card).getByRole('button', { name: 'Open Terminal' })).toBeInTheDocument();
    expect(within(card).getByText('Open the terminal to log in')).toBeInTheDocument();
  });

  it('is one line saying "Log in below" when the terminal is on screen', () => {
    render(
      <ProviderFaultCard
        sessionId="s" failureKind="auth" detail="Claude is not logged in"
        evidence={EVIDENCE} rich={false} placement="banner" onOpenTerminal={() => {}}
      />
    );

    const banner = screen.getByRole('status', { name: 'Provider trouble' });
    expect(within(banner).getByText('Log in below')).toBeInTheDocument();
    expect(within(banner).getByText(EVIDENCE)).toBeInTheDocument();
    // Offering to open the terminal the person is looking at is the part that
    // made this card read as machinery rather than help.
    expect(screen.queryByRole('button', { name: 'Open Terminal' })).not.toBeInTheDocument();
    expect(screen.queryByText('Open the terminal to log in')).not.toBeInTheDocument();
  });
});

describe('capability: the header pill follows the same fact', () => {
  it('says Needs login only while the daemon reports an auth fault', () => {
    const working = makeSession({ id: 'working', tool: 'claude-code', working: true });
    expect(classifySession(working).label).not.toBe('Needs login');

    // The daemon is the only thing that can put a session in this state; the
    // pill reads the same field the card does, so they cannot disagree.
    const faulted = Object.assign(makeSession({ id: 'faulted', tool: 'claude-code' }), {
      failureKind: 'auth' as const,
      failureDetail: 'Claude is not logged in',
      failureEvidence: EVIDENCE
    });
    const status = classifySession(faulted);
    expect(status.label).toBe('Needs login');
    expect(status.state).toBe('auth-needed');
  });
});
