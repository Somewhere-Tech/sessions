import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { NotificationDeliveryCard, NotificationDeliveryStatus } from '../src/components/NotificationDeliveryStatus';
import { useServers } from '../src/lib/servers';
import '../src/styles/globals.css';

const on = { done: true, waiting: true, lost: true };
const off = { done: false, waiting: false, lost: false };
const requests: { url: string; method: string }[] = [];
useServers.setState({ servers: [
  { id: 'mini', name: 'Mac mini', host: 'mini.invalid', port: 8787, isDefault: false },
  { id: 'book', name: 'MacBook', host: 'book.invalid', port: 8787, isDefault: false }
], activeId: 'mini' });
window.fetch = async (input, init) => {
  const url = input instanceof Request ? input.url : String(input);
  requests.push({ url, method: init?.method ?? 'GET' });
  if (url.includes('book.invalid')) throw new Error('This computer is unreachable');
  return new Response(JSON.stringify({ notify: on, subscribed: false }), { headers: { 'content-type': 'application/json' } });
};

function Fixture(): JSX.Element {
  const [server, setServer] = useState('mini');
  return <main className="settings-page" style={{ maxWidth: 820, margin: '32px auto' }}>
    <h1>Notification setup</h1>
    <div id="live"><NotificationDeliveryStatus key={server} serverId={server} hostName={server === 'mini' ? 'Mac mini' : 'MacBook'} /></div>
    <button type="button" onClick={() => setServer('book')}>Check MacBook</button>
    <div id="registered"><NotificationDeliveryCard hostName="Mac mini" error={null} status={{ notify: on, subscribed: true }} /></div>
    <div id="off"><NotificationDeliveryCard hostName="Mac mini" error={null} status={{ notify: off, subscribed: false }} /></div>
  </main>;
}

Object.assign(window, { __notificationRequests: requests });
createRoot(document.getElementById('root')!).render(<Fixture />);
