const TAILNET_CLIENT_ID_KEY = 'sessions:tailnet-client-id';
const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export function tailnetClientID(): string {
  const existing = window.localStorage.getItem(TAILNET_CLIENT_ID_KEY)?.trim().toLowerCase();
  if (existing && UUID_V4.test(existing)) return existing;

  const created = crypto.randomUUID().toLowerCase();
  window.localStorage.setItem(TAILNET_CLIENT_ID_KEY, created);
  return created;
}
