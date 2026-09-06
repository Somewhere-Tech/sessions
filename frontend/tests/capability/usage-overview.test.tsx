import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { UsageOverview } from '../../src/components/UsageDashboard';
import type { UsageRow } from '../../src/api/sessionsd';

function row(): UsageRow {
  return { key: 'today', models: ['astra', 'fable', 'astra'], entries: 4,
    tokens: { inputTokens: 100, cacheCreationTokens: 100, cacheReadTokens: 800, outputTokens: 200, reasoningTokens: 150 },
    costUSD: 0.25, recordedCostUSD: 0.25, calculatedCostUSD: 0, missingPricingEntries: 0 };
}

describe('capability: understand usage without invented productivity claims', () => {
  it('labels missing pricing alongside the headline amount, not just in a footnote', () => {
    const total = row();
    total.missingPricingEntries = 3;
    render(<UsageOverview report={{ totals: total, rows: [total] }} mode="auto" />);
    expect(screen.getByText('Partial cost estimate')).toBeVisible();
    expect(screen.getByText('3 of 4 events unpriced; not your bill')).toBeVisible();
    expect(screen.queryByText('Estimated cost')).not.toBeInTheDocument();
  });

  it('does not call a completely unpriced period free', () => {
    const total = row();
    total.costUSD = 0;
    total.missingPricingEntries = total.entries;
    render(<UsageOverview report={{ totals: total, rows: [total] }} mode="auto" />);
    expect(screen.getByText('Unavailable')).toBeVisible();
    expect(screen.queryByText('$0.000')).not.toBeInTheDocument();
  });

  it('measures context reuse against input, without double-counting reasoning', () => {
    const total = row();
    render(<UsageOverview report={{ totals: total, rows: [total] }} mode="auto" />);
    expect(screen.getByText('80%')).toBeVisible();
    expect(screen.getByText('1,200')).toBeVisible();
    expect(screen.getByText('2')).toBeVisible();
    expect(screen.getByText(/not your bill/)).toBeVisible();
    expect(screen.getByText(/Token volume measures usage, not work quality/)).toBeVisible();
  });

  it('does not invent a reuse percentage for an empty period', () => {
    const total = row();
    total.tokens = { inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, cacheCreationTokens: 0, reasoningTokens: 0 };
    total.models = [];
    render(<UsageOverview report={{ totals: total, rows: [] }} mode="display" />);
    expect(screen.getByText('—')).toBeVisible();
    expect(screen.getByText('No token activity recorded in this period.')).toBeVisible();
    expect(screen.getByText('No model data yet')).toBeVisible();
  });
});
