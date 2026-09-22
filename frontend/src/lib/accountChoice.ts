import type { AccountProfile } from '../api/sessionsd';

// A second subscription is a normal choice, so it is remembered the way a
// person thinks about it: per project, per provider, on the machine it lives
// on. Nothing here holds a credential — an account is a name and a label.

const STORAGE_KEY = 'sessions:account-choice';

export type AccountProvider = 'claude' | 'codex';

function readAll(): Record<string, string> {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : {};
    return parsed && typeof parsed === 'object' ? parsed as Record<string, string> : {};
  } catch {
    return {};
  }
}

function choiceKey(machineId: string, provider: AccountProvider, project: string): string {
  return `${machineId} ${provider} ${project}`;
}

/** The account last chosen for this project on this machine, or none. */
export function rememberedAccount(machineId: string, provider: AccountProvider, project: string): string {
  if (!project) return '';
  return readAll()[choiceKey(machineId, provider, project)] ?? '';
}

export function rememberAccount(
  machineId: string, provider: AccountProvider, project: string, account: string
): void {
  if (!project) return;
  const all = readAll();
  if (account) {
    all[choiceKey(machineId, provider, project)] = account;
  } else {
    delete all[choiceKey(machineId, provider, project)];
  }
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(all));
  } catch { /* a device that refuses storage still gets the default */ }
}

/**
 * What to call an account in a menu or a badge: the label its owner typed, then
 * the name they gave the home, and never anything read out of a credential.
 */
export function accountLabel(profile: AccountProfile): string {
  return profile.label?.trim() || profile.identity?.email || profile.name;
}

/** The same, for a name with no profile record to hand (a session's own). */
export function accountLabelOf(profiles: AccountProfile[], provider: AccountProvider, name: string): string {
  const match = profiles.find((profile) => profile.tool === provider && profile.name === name);
  return match ? accountLabel(match) : name;
}

/** An account that exists but has no provider login yet is worth saying so. */
export function accountNeedsLogin(profiles: AccountProfile[], provider: AccountProvider, name: string): boolean {
  const match = profiles.find((profile) => profile.tool === provider && profile.name === name);
  return Boolean(match && !match.identity && !match.signed_in);
}
