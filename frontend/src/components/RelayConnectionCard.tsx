import { useEffect, useState } from 'react';
import { fetchRelayState, setRelayURL } from '../api/sessionsd';

export function RelayConnectionCard(): JSX.Element {
  const [url, setURL] = useState('');

  useEffect(() => {
    void fetchRelayState().then((state) => setURL(state.url)).catch(() => {});
  }, []);

  const save = (): void => {
    void setRelayURL(url.trim()).then((saved) => setURL(saved.url))
      .catch((reason) => window.alert(String(reason)));
  };

  return (
    <section className="settings-card">
      <h2>Relay fallback</h2>
      <label><span>URL</span><input className="settings-menu-input" type="url" placeholder="https://relay" value={url} onChange={(event) => setURL(event.currentTarget.value)} /></label>
      <button type="button" className="btn" onClick={save}>Save</button>
    </section>
  );
}
