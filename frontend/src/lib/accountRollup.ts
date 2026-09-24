import type { AccountIdentity, AccountProfile, AccountUsage } from '../api/sessionsd';

// One account, however many computers it is signed into.
//
// A subscription's allowance belongs to the provider account, not to the
// computer, so the Accounts page leads with accounts. What makes two computers'
// homes "the same account" is the provider's own stable account ID. An email is
// not enough: one person can belong to several ChatGPT workspaces or Claude
// organizations under one email, each with its own allowance. Homes without a
// shared provider ID therefore stay separate rows, and a matching email is only
// mentioned, never merged.
//
// Usage is never added across computers. Every computer reads the same
// provider allowance, so the freshest reading wins and the others are coverage.

export interface MachineAccounts {
  serverId: string;
  machineName: string;
  /** Null while the computer has not answered yet. */
  profiles: AccountProfile[] | null;
  profilesError?: string;
  /** Undefined until read; null when that computer's Sessions cannot read usage. */
  usage?: AccountUsage[] | null;
  usageError?: string;
}

export interface AccountPlacement {
  serverId: string;
  machineName: string;
  profile: AccountProfile;
  /** This computer's reading for this account, when it gave one. */
  usage?: AccountUsage;
  /** Why this computer has no reading. */
  usageGap?: 'older-sessions' | 'not-answered' | 'pending';
}

export interface AccountReading {
  usage: AccountUsage;
  serverId: string;
  machineName: string;
}

export interface AccountGroup {
  key: string;
  tool: 'claude' | 'codex';
  /** A provider account ID ties every placement here to one allowance. */
  matchedByProvider: boolean;
  identity?: AccountIdentity;
  placements: AccountPlacement[];
  /** The freshest reading of this allowance, from whichever computer read it. */
  reading?: AccountReading;
  /** Computers holding an unmatched account with the same email. */
  sameEmailOn: string[];
}

/** The freshest identity a computer has for this home: its read, or its saved check. */
export function placementIdentity(placement: AccountPlacement): AccountIdentity | undefined {
  const saved = placement.profile.identity;
  const read = placement.usage?.identity;
  if (!read) return saved;
  if (!saved) return read;
  return read.checked_at >= saved.checked_at ? read : saved;
}

function groupKey(placement: AccountPlacement): string {
  const accountId = placementIdentity(placement)?.account_id?.trim();
  if (accountId) return `${placement.profile.tool}:account:${accountId}`;
  return `${placement.profile.tool}:home:${placement.serverId}:${placement.profile.name}`;
}

function placementsOf(machine: MachineAccounts): AccountPlacement[] {
  return (machine.profiles ?? []).map((profile) => {
    const usage = machine.usage?.find((item) => item.tool === profile.tool && item.name === profile.name);
    let usageGap: AccountPlacement['usageGap'];
    if (!usage) {
      if (machine.usage === null) usageGap = 'older-sessions';
      else if (machine.usageError) usageGap = 'not-answered';
      else usageGap = 'pending';
    }
    return { serverId: machine.serverId, machineName: machine.machineName, profile, usage, usageGap };
  });
}

/** Freshest non-stale reading, else the freshest stale one; never a sum. */
export function freshestReading(placements: AccountPlacement[]): AccountReading | undefined {
  let best: AccountReading | undefined;
  const rank = (usage: AccountUsage): number => (usage.stale ? 0 : 1);
  for (const placement of placements) {
    const usage = placement.usage;
    if (!usage?.read_at || !usage.buckets || (usage.state !== 'available' && !usage.stale)) continue;
    const better = !best
      || rank(usage) > rank(best.usage)
      || (rank(usage) === rank(best.usage) && usage.read_at > (best.usage.read_at ?? 0));
    if (better) best = { usage, serverId: placement.serverId, machineName: placement.machineName };
  }
  return best;
}

function freshestIdentity(placements: AccountPlacement[]): AccountIdentity | undefined {
  let best: AccountIdentity | undefined;
  for (const placement of placements) {
    const identity = placementIdentity(placement);
    if (identity && (!best || identity.checked_at > best.checked_at)) best = identity;
  }
  return best;
}

/** Group every computer's accounts into one row per provable account. */
export function rollUpAccounts(machines: MachineAccounts[]): AccountGroup[] {
  const groups = new Map<string, AccountGroup>();
  for (const machine of machines) {
    for (const placement of placementsOf(machine)) {
      const key = groupKey(placement);
      const group = groups.get(key) ?? {
        key, tool: placement.profile.tool, matchedByProvider: key.includes(':account:'),
        placements: [], sameEmailOn: []
      };
      group.placements.push(placement);
      groups.set(key, group);
    }
  }
  const all = [...groups.values()];
  for (const group of all) {
    group.identity = freshestIdentity(group.placements);
    group.reading = freshestReading(group.placements);
  }
  for (const group of all) {
    const email = group.identity?.email?.toLowerCase();
    if (!email) continue;
    group.sameEmailOn = [...new Set(all
      .filter((other) => other !== group && other.tool === group.tool && other.identity?.email?.toLowerCase() === email)
      .flatMap((other) => other.placements.map((placement) => placement.machineName)))];
  }
  // Computer order, then each computer's own order: a rename never moves a row.
  return all;
}

const providerName = (tool: 'claude' | 'codex'): string => tool === 'codex' ? 'ChatGPT' : 'Claude';

/** A row leads with the owner's nickname, then the verified email. */
export function groupTitle(group: AccountGroup): string {
  const nickname = group.placements.map((placement) => placement.profile.label?.trim()).find(Boolean);
  return nickname || group.identity?.email || `${providerName(group.tool)} account`;
}

/**
 * Computers this account could be added to: reachable ones that do not already
 * hold a home of the same provider and name, so adding never reuses a home that
 * might be signed into something else.
 */
export function computersWithout(group: AccountGroup, machines: MachineAccounts[]): MachineAccounts[] {
  const name = group.placements[0]?.profile.name;
  return machines.filter((machine) => machine.profiles !== null && !machine.profilesError
    && !group.placements.some((placement) => placement.serverId === machine.serverId)
    && !machine.profiles.some((profile) => profile.tool === group.tool && profile.name === name));
}

/** What the page can and cannot see across computers, said once. */
export function coverageNotes(machines: MachineAccounts[]): string[] {
  const notes: string[] = [];
  for (const machine of machines) {
    if (machine.profilesError) {
      notes.push(`${machine.machineName} did not answer, so its accounts are not shown.`);
    } else if (machine.usage === null && (machine.profiles?.length ?? 0) > 0) {
      notes.push(`Update Sessions on ${machine.machineName} to see usage from it.`);
    }
  }
  return notes;
}
