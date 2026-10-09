import type { AccountIdentity, AccountProfile, AccountUsage } from '../api/sessionsd';

// One account, however many computers it is signed into.
//
// A subscription's allowance belongs to the provider account, not to the
// computer, so the Accounts page leads with accounts. What makes two computers'
// homes "the same account" is the provider's own stable account ID. An email is
// not enough: one person can belong to several ChatGPT workspaces or Claude
// organizations under one email, each with its own allowance. Homes without a
// shared provider ID therefore stay separate rows, and a matching email is only
// mentioned, never merged. No supported provider reports such an ID today, so
// until a verified source exists every computer's home is its own row.
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

/**
 * When this home's most recent check did not confirm an identity: signed out,
 * another kind of login, a failed check or a re-add. Anything this computer
 * observed before then is history. Zero when the latest check verified.
 */
export function supersededAt(profile: AccountProfile): number {
  const check = profile.last_check;
  return !profile.identity && check && check.outcome !== 'verified' ? check.at : 0;
}

/** What one computer last established about who is signed in to its home. */
export type PlacementCheck =
  | { kind: 'signed_in'; at: number; identity?: AccountIdentity }
  | { kind: 'verified'; at: number; identity: AccountIdentity }
  | { kind: 'signed_out' | 'not_subscription' | 'failed'; at?: number; message?: string; previous?: AccountIdentity }
  | { kind: 'unchecked'; at?: number; previous?: AccountIdentity };

type PositiveCheck = Extract<PlacementCheck, { kind: 'signed_in' | 'verified' }>;
const positive = (check: PlacementCheck): check is PositiveCheck => check.kind === 'signed_in' || check.kind === 'verified';

/**
 * Which observation wins when two were made in the same millisecond: an answer
 * that the home is signed out or on another login, then a failed check or a
 * re-add, then a successful read, then a saved verification. Conservative: a
 * cached positive reading never contradicts a negative or unknown check made
 * at the same time.
 */
const tieRank: Record<PlacementCheck['kind'], number> = {
  signed_out: 3, not_subscription: 3, failed: 2, unchecked: 2, signed_in: 1, verified: 0
};

/**
 * Everything this computer observed about the home, each with its time: the
 * saved check (a verified identity or a newer outcome that superseded it) and
 * the usage read (a successful read, a signed-out answer, or the identity an
 * otherwise unavailable read reported). An undated answer is the oldest.
 *
 * A usage read asks who is signed in first and reads the limits afterwards, so
 * its identity is dated by the identity's own `checked_at`, not by the later
 * limits answer: a limits answer alone does not make an identity read before a
 * newer check current again. The two steps are not one atomic answer.
 */
function observationsOf(placement: AccountPlacement): PlacementCheck[] {
  const { usage, profile } = placement;
  const previous = profile.previous_identity;
  const found: PlacementCheck[] = [];
  const check = profile.last_check;
  if (profile.identity) {
    const at = check?.outcome === 'verified' ? Math.max(check.at, profile.identity.checked_at) : profile.identity.checked_at;
    found.push({ kind: 'verified', at, identity: profile.identity });
  } else if (check && check.outcome !== 'verified') {
    const { at, outcome } = check;
    found.push(outcome === 'signed_out' || outcome === 'not_subscription' || outcome === 'failed'
      ? { kind: outcome, at, previous }
      : { kind: 'unchecked', at, previous });
  }
  if (usage?.state === 'available' && !usage.stale && usage.checked_at) {
    found.push({ kind: 'signed_in', at: usage.identity?.checked_at ?? usage.checked_at, identity: usage.identity });
  } else if (usage?.state === 'signed_out') {
    found.push({ kind: 'signed_out', at: usage.checked_at, message: usage.message, previous });
  } else if (usage?.identity) {
    found.push({ kind: 'verified', at: usage.identity.checked_at, identity: usage.identity });
  }
  return found;
}

const observedAt = (check: PlacementCheck): number => check.at ?? Number.NEGATIVE_INFINITY;

/**
 * The freshest thing this computer established about its home. A usage read
 * that succeeded is a provider check; a saved identity is a check that
 * happened once; a login file on disk is only a file, so it never reads as
 * ready. Whichever observation is newest wins, ties follow `tieRank`, and a
 * failed check is an unknown, not a sign-out.
 */
export function placementCheck(placement: AccountPlacement): PlacementCheck {
  let best: PlacementCheck | undefined;
  for (const check of observationsOf(placement)) {
    const newer = !best || observedAt(check) > observedAt(best)
      || (observedAt(check) === observedAt(best) && tieRank[check.kind] > tieRank[best.kind]);
    if (newer) best = check;
  }
  return best ?? { kind: 'unchecked', previous: placement.profile.previous_identity };
}

/**
 * The identity this computer currently has for the home, consistent with
 * `placementCheck`: none unless the freshest observation is a positive one,
 * and then the freshest identity any positive observation reported.
 */
export function placementIdentity(placement: AccountPlacement): AccountIdentity | undefined {
  if (!positive(placementCheck(placement))) return undefined;
  let best: AccountIdentity | undefined;
  for (const check of observationsOf(placement)) {
    if (positive(check) && check.identity && (!best || check.identity.checked_at > best.checked_at)) best = check.identity;
  }
  return best;
}

/**
 * Whether two reported identities are known to be different accounts: a
 * different stable provider ID when both have one, otherwise a different email
 * or organization. A matching email is not proof of the same subscription; it
 * only means no difference is known.
 */
function knownDifferent(left: AccountIdentity, right: AccountIdentity): boolean {
  const leftId = left.account_id?.trim();
  const rightId = right.account_id?.trim();
  if (leftId && rightId) return leftId !== rightId;
  if (left.email.trim().toLowerCase() !== right.email.trim().toLowerCase()) return true;
  return Boolean(left.organization && right.organization && left.organization !== right.organization);
}

/**
 * Whether this computer's usage reading may be shown as the account's
 * allowance. Not when a newer check that did not confirm the sign-in came
 * after the identity the reading was taken under (or after the reading, when
 * it reported none), and not when that identity is known to differ from the
 * account this home is currently verified as.
 */
function readingBelongs(placement: AccountPlacement, usage: AccountUsage): boolean {
  const barrier = supersededAt(placement.profile);
  const observed = usage.identity?.checked_at ?? usage.read_at ?? 0;
  if (observed <= barrier || (usage.read_at ?? 0) <= barrier) return false;
  const current = placementIdentity(placement);
  return !(usage.identity && current && knownDifferent(usage.identity, current));
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
    if (!readingBelongs(placement, usage)) continue;
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

/** A row leads with the owner's nickname, then the currently verified email. */
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
