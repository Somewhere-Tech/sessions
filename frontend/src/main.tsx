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

const rootElement = document.getElementById('root');
if (!rootElement) throw new Error('Sessions could not find its application root.');
const root = ReactDOM.createRoot(rootElement);

function StartupShell(): JSX.Element {
  const [slow, setSlow] = React.useState(false);

  React.useEffect(() => {
    const timer = window.setTimeout(() => setSlow(true), 8_000);
    return () => window.clearTimeout(timer);
  }, []);

  return (
    <main className="startup-shell" aria-live="polite">
      <section className="startup-card">
        <div className="startup-mark" aria-hidden>S</div>
        <p className="startup-kicker">Sessions</p>
        <h1>Opening your workspace…</h1>
        <p>Your agents run in the background and are not interrupted by this window.</p>
        <span className="startup-progress" aria-hidden />
        {slow ? (
          <div className="startup-slow">
            <strong>The window is taking longer than usual.</strong>
            <span>Your sessions are still safe. Reloading only restarts this view.</span>
            <button type="button" onClick={() => window.location.reload()}>Reload window</button>
          </div>
        ) : null}
      </section>
    </main>
  );
}

interface AppBoundaryState {
  error: Error | null;
}

class AppBoundary extends React.Component<React.PropsWithChildren, AppBoundaryState> {
  state: AppBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): AppBoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo): void {
    console.error('Sessions interface failed to render', error, info);
  }

  render(): React.ReactNode {
    if (!this.state.error) return this.props.children;
    return (
      <main className="startup-shell" role="alert">
        <section className="startup-card startup-recovery">
          <div className="startup-mark" aria-hidden>S</div>
          <p className="startup-kicker">Display problem</p>
          <h1>Sessions is still running.</h1>
          <p>
            This window could not finish drawing. Your background service and
            agent sessions were not stopped.
          </p>
          <button type="button" onClick={() => window.location.reload()}>Reload window</button>
        </section>
      </main>
    );
  }
}

function renderApp(): void {
  root.render(
    <React.StrictMode>
      <AppBoundary>
        <App />
      </AppBoundary>
    </React.StrictMode>
  );
}

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
}

root.render(<StartupShell />);
void bootstrap()
  .catch((error: unknown) => {
    const detail = error instanceof Error ? error.message : String(error);
    console.error('Sessions startup failed', error);
    useServers.getState().setPairingError(
      `Sessions could not finish connecting this window: ${detail}. Your sessions are still running. Choose a remembered machine or reconnect to try again.`
    );
  })
  .finally(renderApp);
