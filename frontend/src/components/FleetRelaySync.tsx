import { useFleetRelayServers } from '../lib/fleetRelay';

export function FleetRelaySync(): JSX.Element | null {
  const errors = useFleetRelayServers(true);
  if (!errors.length) return null;
  return <details className="fleet-refresh-notice" role="status">
    <summary>Some computers couldn’t refresh. Saved connections are still available.</summary>
    {errors.map((error) => <div key={error}>{error}</div>)}
  </details>;
}
