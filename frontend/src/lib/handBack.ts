import type { SessionInfo } from '../types';
import { resolvedSessionLabel } from './tabLabels';

// handBackMessage is what a lane posts into its manager's conversation when
// its work is handed back: the last line the lane said or is waiting on, its
// id for a deeper read, and the branch its work is on so the manager knows
// what to diff or merge. Attributed to the lane by the caller.
export function handBackMessage(lane: SessionInfo): string {
  const line = (lane.idleReason === 'needs-input' && lane.idleDetail)
    ? `is waiting: ${lane.idleDetail}`
    : lane.lastSummary?.trim()
      ? `reports: ${lane.lastSummary.trim().split('\n')[0]}`
      : 'has nothing to report yet';
  const where = lane.branch ? ` Its work is on branch ${lane.branch}${lane.worktreePath ? ` in ${lane.worktreePath}` : ''}.` : '';
  const short = lane.id.slice(0, 8);
  return `Lane "${resolvedSessionLabel(lane)}" ${line} (id ${short}; \`sessions cat ${short}\` for the full conversation).${where}`;
}
