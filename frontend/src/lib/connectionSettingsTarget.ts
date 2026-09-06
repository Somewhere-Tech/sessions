import { getActiveServer, isLocalServer, useServers, type ServerConfig } from './servers';
import { isNativeMobileRuntime, isTauri } from './tauriBridge';

// Desktop connection controls configure this installation, not the machine
// whose conversation the user most recently opened. Phones inherit their host.
export function connectionSettingsTarget(): ServerConfig {
  if (!isTauri() || isNativeMobileRuntime()) return getActiveServer();
  const local = useServers.getState().servers.find((server) => server.isDefault && isLocalServer(server));
  if (!local) throw new Error('This computer’s connection is not ready. Reopen Sessions and try again.');
  // Administrative routes require loopback. A discovered LAN/tailnet route
  // for this same computer is useful for peers, not for its own Settings.
  return { ...local, transportCandidates: undefined };
}
