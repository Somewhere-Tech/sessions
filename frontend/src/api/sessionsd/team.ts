import { apiFetch, httpBase, json } from './core';
import type { StartReceipt } from '../../types';

export type TeamMemberState = 'ended' | 'needs-you' | 'working' | 'failed' | 'not-started' | 'idle' | 'lost' | 'unreachable' | 'needs-recovery';

export interface HandoffReceipt {
  source: 'not-reported' | 'agent-reported';
  seq?: number;
  at?: string;
  outcome?: string;
  summary?: string;
  workspace?: string;
  branch?: string;
  commits?: string[];
  push: 'unknown' | 'pushed' | 'not-pushed';
  tests?: string[];
  artifacts?: string[];
  remaining?: string[];
  detail?: string;
}

export interface CheckoutWarning {
  path?: string;
  dirty?: boolean;
  sessions?: string[];
  detail: string;
}

export interface TeamMember {
  start?: StartReceipt;
  id: string;
  name?: string;
  tool: string;
  cwd?: string;
  relation: 'self' | 'parent' | 'child';
  depth: number;
  state: TeamMemberState;
  needs_you: boolean;
  working: boolean;
  exited: boolean;
  summary?: string;
  waiting?: string;
  updated_at?: number;
  branch?: string;
  worktree_path?: string;
  handoff?: HandoffReceipt;
  checkout_warning?: CheckoutWarning;
}

export interface TeamListing {
  self?: TeamMember;
  parent?: TeamMember;
  members: TeamMember[];
  needs_input: number;
  next_cursor?: string;
  delta?: boolean;
  total?: number;
  removed?: string[];
}

export async function fetchTeam(laneId: string, signal?: AbortSignal, since?: string): Promise<TeamListing> {
  const query = new URLSearchParams({ lane: laneId });
  if (since) query.set('since', since);
  const response = await apiFetch(`${httpBase()}/api/lanes/mine?${query.toString()}`, { signal });
  return json<TeamListing>(response);
}
