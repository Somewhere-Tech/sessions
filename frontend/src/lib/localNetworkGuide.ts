// What the nearby-access guide may claim, given what the host reported and what
// the last explicit check actually observed.
//
// The host's `permission.status` is its last observation, not a live reading:
// macOS exposes no API for the Local Network switch, so `granted` only records
// that nearby contact succeeded at some point. Presenting that as a current
// connection told people access was working while their check had just failed.
// Fresh contact outranks the stored observation, and so does fresh failure.

export type LocalNetworkCheck =
  // No explicit check has been made from this surface yet, or the check only
  // re-read a remote host's observation and proved nothing itself.
  | 'none'
  // The daemon reached at least one nearby machine during this check.
  | 'reached'
  // The check completed and nothing answered. An empty discovery is not proof
  // that access works, whatever the stored observation says.
  | 'none-found'
  // The check itself failed. Its error is fresh evidence and must stay visible.
  | 'failed';

export type LocalNetworkGuideMode = 'hidden' | 'confirmed' | 'previously-worked' | 'guide';

export interface LocalNetworkGuideInput {
  status?: string;
  /** This surface has seen the host report something other than a success. */
  sawUnproven: boolean;
  lastCheck: LocalNetworkCheck;
}

export function localNetworkGuideMode({ status, sawUnproven, lastCheck }: LocalNetworkGuideInput): LocalNetworkGuideMode {
  if (!status || status === 'not-required') return 'hidden';
  if (lastCheck === 'reached') return 'confirmed';
  if (lastCheck === 'failed' || lastCheck === 'none-found') return 'guide';
  if (status === 'granted') return sawUnproven ? 'previously-worked' : 'hidden';
  return status === 'denied' || status === 'not-yet-asked' ? 'guide' : 'hidden';
}
