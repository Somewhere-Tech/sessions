import { useFleetRelayServers } from '../lib/fleetRelay';

export function FleetRelaySync(): null {
  useFleetRelayServers(true);
  return null;
}
