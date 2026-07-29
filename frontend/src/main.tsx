import React from 'react';
import ReactDOM from 'react-dom/client';
import { App } from './App';
import {
  bootstrapHostedConnection,
  bootstrapPairingConnection
} from './lib/hostedBootstrap';
import {
  blockNativeMachineCredentialPersistence,
  bootstrapCurrentOriginServer,
  hydrateNativeMachineCredentials,
  useServers
} from './lib/servers';
import './styles/globals.css';
import './styles/utilities.css';

async function bootstrap(): Promise<void> {
  let credentialHydrationFailed = false;
  try {
    await hydrateNativeMachineCredentials();
  } catch (error) {
    const detail = error instanceof Error
      ? error.message
      : 'Windows could not unlock the saved machine credentials.';
    blockNativeMachineCredentialPersistence(detail);
    credentialHydrationFailed = true;
    const store = useServers.getState();
    store.setCredentialError(
      `${detail} Sessions stopped before contacting a machine. Reopen the app as the Windows user who saved these machines. If it still fails, revoke this Windows device on each host before clearing its local Sessions credential vault and pairing again.`
    );
    store.setActive(null);
  }

  if (!credentialHydrationFailed) {
    // Pairing is same-origin and authoritative. Claim (and scrub) it before a
    // hosted endpoint fragment or the current-origin health probe can run.
    const pairFragmentPresent = await bootstrapPairingConnection();
    // Fragment connections are authoritative and must be applied (and
    // scrubbed) before considering whether this page is a daemon's own
    // non-8787 UI.
    const endpointFragmentPresent = new URLSearchParams(
      window.location.hash.slice(1)
    ).has('endpoint');
    if (!pairFragmentPresent) await bootstrapHostedConnection();
    if (!pairFragmentPresent && !endpointFragmentPresent) {
      await bootstrapCurrentOriginServer();
    }
  }

  ReactDOM.createRoot(document.getElementById('root')!).render(
    <React.StrictMode>
      <App />
    </React.StrictMode>
  );
}

void bootstrap();
