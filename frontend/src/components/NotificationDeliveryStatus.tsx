import { useEffect, useState } from 'react';
import { fetchNotificationDeliveryStatus, type NotificationDeliveryStatus as DeliveryStatus } from '../api/sessionsd';

export function NotificationDeliveryStatus({ serverId, hostName }: { serverId: string | null; hostName: string }): JSX.Element {
  const [status, setStatus] = useState<DeliveryStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    setStatus(null);
    setError(null);
    if (!serverId) {
      setError('Choose a connected computer to check its notification setup.');
      return () => controller.abort();
    }
    void fetchNotificationDeliveryStatus(serverId, controller.signal).then((next) => {
      if (!controller.signal.aborted) setStatus(next);
    }).catch((failure: unknown) => {
      if (!controller.signal.aborted) setError(failure instanceof Error ? failure.message : 'Could not read push delivery status.');
    });
    return () => controller.abort();
  }, [serverId]);
  return <NotificationDeliveryCard status={status} error={error} hostName={hostName} />;
}

export function NotificationDeliveryCard({ status, error, hostName }: { status: DeliveryStatus | null; error: string | null; hostName: string }): JSX.Element {
  const enabled = status ? [status.notify.done && 'Completed work', status.notify.waiting && 'Waiting for you', status.notify.lost && 'Lost contact'].filter(Boolean) : [];
  return (
    <div className="settings-card" aria-label="Push delivery status">
      <h2>Web push notifications</h2>
      <p>Preferences and a receiving device are both needed for push alerts from {hostName}.</p>
      {error ? <p role="status">Could not check delivery: {error} No notification setting was changed.</p> : !status ? <p role="status">Checking preferences and receiving devices…</p> : (
        <>
          <div className="settings-static-row"><span><strong>Send alerts for</strong><small>{enabled.length ? enabled.join(' · ') : 'All push alerts are switched off'}</small></span></div>
          <div className="settings-static-row"><span><strong>{status.subscribed ? 'Receiving device registered' : 'No receiving device registered'}</strong><small>
            {status.subscribed ? 'A push destination is configured on this computer. This is not a delivery test or confirmation that this device receives alerts.' : enabled.length ? 'Your preferences are on, but there is nowhere to send push alerts yet.' : 'No push destination is configured on this computer.'}
          </small></span></div>
          {!status.subscribed ? <p>On the device that should receive alerts, open this computer’s Sessions web view over HTTPS, then turn on Push notifications in the settings menu and allow the browser’s notification request. Native app notifications are separate.</p> : null}
        </>
      )}
    </div>
  );
}
