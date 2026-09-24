import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { fetchAccountUsage, fetchProfiles, type AccountProfile } from '../api/sessionsd';
import type { MachineAccounts } from '../lib/accountRollup';
import { serverDisplayName, useServers } from '../lib/servers';

// Every computer's accounts and their usage readings, read the way Fleet reads
// them: each computer answers for itself, and one that does not answer is a
// note on the page rather than a reason to hide the others.

interface Options {
  /** The computer the page was opened for; its list arrives from the caller. */
  homeId: string;
  homeName: string;
  homeProfiles: AccountProfile[];
  onHomeReload?: (profiles: AccountProfile[]) => void;
}

type MachineState = Partial<Omit<MachineAccounts, 'serverId' | 'machineName'>>;

function message(reason: unknown, fallback: string): string {
  return reason instanceof Error && reason.message ? reason.message : fallback;
}

export function useAccountFleet({ homeId, homeName, homeProfiles, onHomeReload }: Options) {
  const servers = useServers((state) => state.servers);
  const computers = useMemo(() => {
    const listed = servers.filter((server) => !server.directoryOnly)
      .map((server) => ({ id: server.id, name: server.id === homeId ? homeName : serverDisplayName(server, true) }));
    const home = listed.find((computer) => computer.id === homeId) ?? { id: homeId, name: homeName };
    // The page's own computer leads, so its accounts come first.
    return [home, ...listed.filter((computer) => computer.id !== home.id)];
  }, [servers, homeId, homeName]);
  const [state, setState] = useState<Record<string, MachineState>>({});
  const [refreshing, setRefreshing] = useState(false);
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; }, []);
  const patch = useCallback((id: string, next: Partial<MachineState>): void => {
    if (!alive.current) return;
    setState((current) => ({ ...current, [id]: { ...current[id], ...next } }));
  }, []);

  const readUsage = useCallback(async (id: string, refresh = false): Promise<void> => {
    try {
      patch(id, { usage: await fetchAccountUsage(id || undefined, { refresh }), usageError: undefined });
    } catch (reason) {
      patch(id, { usageError: message(reason, 'That computer did not answer.') });
    }
  }, [patch]);

  const readProfiles = useCallback(async (id: string): Promise<AccountProfile[] | null> => {
    try {
      const profiles = await fetchProfiles(undefined, id || undefined);
      patch(id, { profiles, profilesError: undefined });
      if (id === homeId) onHomeReload?.(profiles);
      return profiles;
    } catch (reason) {
      patch(id, { profilesError: message(reason, 'That computer did not answer.') });
      return null;
    }
  }, [patch, homeId, onHomeReload]);

  // The caller's list is the page computer's truth; others are read here.
  useEffect(() => { patch(homeId, { profiles: homeProfiles, profilesError: undefined }); }, [patch, homeId, homeProfiles]);
  const computerKey = computers.map((computer) => computer.id).join('\n');
  useEffect(() => {
    for (const computer of computers) {
      if (computer.id !== homeId) void readProfiles(computer.id);
      void readUsage(computer.id);
    }
    // Re-read only when the set of computers changes, not on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [computerKey]);

  const machines: MachineAccounts[] = computers.map((computer) => ({
    ...state[computer.id], serverId: computer.id, machineName: computer.name, profiles: state[computer.id]?.profiles ?? null
  }));

  /** Re-read one computer after a change made there. */
  const reloadComputer = useCallback(async (id: string): Promise<void> => {
    await readProfiles(id);
    await readUsage(id);
  }, [readProfiles, readUsage]);

  /**
   * Take a computer's fresh list after a sign-in there, and ask its providers
   * again: the reading from before the sign-in no longer describes it.
   */
  const accept = useCallback((id: string, profiles: AccountProfile[]): void => {
    patch(id, { profiles, profilesError: undefined });
    if (id === homeId) onHomeReload?.(profiles);
    void readUsage(id, true);
  }, [patch, homeId, onHomeReload, readUsage]);

  /** Ask every reachable computer's providers again. */
  const refreshUsage = useCallback(async (): Promise<void> => {
    setRefreshing(true);
    try {
      await Promise.all(computers.map((computer) => readUsage(computer.id, true)));
    } finally {
      if (alive.current) setRefreshing(false);
    }
  }, [computers, readUsage]);

  return { machines, accept, reloadComputer, refreshUsage, refreshing };
}
