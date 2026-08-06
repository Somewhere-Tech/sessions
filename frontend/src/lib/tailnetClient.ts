const TAILNET_CLIENT_ID_KEY = 'sessions:tailnet-client-id';
const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

// A per-session fallback for storage-denied contexts (Safari private mode, a
// locked-down WebView, a browser with site data blocked). It keeps the ID
// stable for the lifetime of the page so the poll that follows a "Request
// access" click still matches the request it created; it is simply not
// remembered across reloads, which costs the user one extra approval.
let ephemeralClientID: string | null = null;

function readStored(): string | null {
  try {
    return window.localStorage.getItem(TAILNET_CLIENT_ID_KEY);
  } catch {
    // Storage access can throw, not just return null.
    return null;
  }
}

function writeStored(value: string): void {
  try {
    window.localStorage.setItem(TAILNET_CLIENT_ID_KEY, value);
  } catch {
    // Identity is a convenience, not a credential. Losing it across reloads
    // is acceptable; throwing is not — this runs inside the three
    // "Request access" click handlers (ConnectScreen, FleetView,
    // ConnectionsView), none of which catch, so an unguarded throw made the
    // button do nothing at all with no message.
  }
}

export function tailnetClientID(): string {
  const existing = readStored()?.trim().toLowerCase();
  if (existing && UUID_V4.test(existing)) return existing;
  if (ephemeralClientID) return ephemeralClientID;

  const created = crypto.randomUUID().toLowerCase();
  ephemeralClientID = created;
  writeStored(created);
  return created;
}
