import { apiFetch, httpBase, json } from './core';

export type TeamMemberState = 'ended' | 'needs-you' | 'working' | 'failed' | 'not-started' | 'idle';

export interface TeamMember {
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
}

export interface TeamListing {
  self: TeamMember;
  parent?: TeamMember;
  members: TeamMember[];
  needs_input: number;
}

export async function fetchTeam(laneId: string, signal?: AbortSignal): Promise<TeamListing> {
  const query = new URLSearchParams({ lane: laneId });
  const response = await apiFetch(`${httpBase()}/api/lanes/mine?${query.toString()}`, { signal });
  return json<TeamListing>(response);
}
