// sessions app-shell service worker.
//
// Goals:
//   1. Tap the home-screen icon → app opens INSTANTLY from cache (no
//      network round-trip blocking first paint).
//   2. /api/* and /ws are NEVER intercepted — those must always hit
//      sessionsd live; caching a session list or replaying a stale WS
//      handshake would be a footgun.
//   3. Survive offline / sessionsd-not-yet-up: the bundle still loads
//      from cache, the React app boots, hydrates from localStorage
//      (last seen sessions list, lastSeq per session), and shows the
//      familiar tabs even before the WS connects.
//
// Strategy:
//   • install: precache a tiny static shell (just enough to render
//     "loading…").
//   • fetch:
//       /api/* or /ws/*       → bypass entirely (let browser network it).
//       navigation requests   → network-first, fall back to cached
//                               /index.html when offline. Also opportun-
//                               istically refreshes the cached index so
//                               next cold-load gets the latest HTML.
//       same-origin GET asset → cache-first with background refresh
//                               (stale-while-revalidate). Hashed asset
//                               names mean the cached entry is naturally
//                               immutable for that hash; new builds add
//                               new entries.
//   • activate: drop any old caches whose key isn't the current version.

// Vite replaces this marker in the emitted sw.js with a unique build hash,
// so every production build evicts the previous app-shell cache.
const CACHE_VERSION = 'sessions-__BUILD_HASH__';
const SCOPE_URL = self.registration.scope;
const scoped = (path) => new URL(path, SCOPE_URL).href;
const INDEX_URL = scoped('index.html');
const SHELL = [scoped('./'), INDEX_URL, scoped('manifest.webmanifest'), scoped('icon.svg'), scoped('icon-maskable.svg')];

self.addEventListener('install', (event) => {
  // skipWaiting() lets the new SW take over immediately without waiting for
  // all existing tabs to close.  The activate handler posts a 'sw-update-ready'
  // message so the page can show an "update available — reload to apply" banner
  // rather than silently replacing a live session mid-session.
  self.skipWaiting();
  event.waitUntil(
    caches.open(CACHE_VERSION).then((cache) => cache.addAll(SHELL).catch(() => {}))
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE_VERSION).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
      .then(() => self.clients.matchAll({ type: 'window' }))
      .then((clients) => {
        // Notify open tabs that a new SW version is active so the UI can
        // prompt the user to reload at a safe moment.
        for (const client of clients) {
          client.postMessage({ type: 'sw-update-ready', version: CACHE_VERSION });
        }
      })
  );
});

self.addEventListener('push', (event) => {
  var payload = {};
  if (event.data) {
    try {
      payload = event.data.json();
    } catch {
      payload = { title: 'Session finished', body: event.data.text() };
    }
  }
  var data = payload && typeof payload.data === 'object' && payload.data
    ? payload.data
    : {};
  var sessionId = typeof data.sessionId === 'string' ? data.sessionId : undefined;
  event.waitUntil(
    self.registration.showNotification(
      typeof payload.title === 'string' ? payload.title : 'Session finished',
      {
        body: typeof payload.body === 'string' ? payload.body : undefined,
        data: data,
        icon: scoped('icon.svg'),
        tag: sessionId
      }
    )
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  var data = event.notification.data && typeof event.notification.data === 'object'
    ? event.notification.data
    : {};
  var sessionId = typeof data.sessionId === 'string' ? data.sessionId : null;
  var message = sessionId ? { type: 'push-open-session', sessionId: sessionId } : null;

  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((clients) => {
      if (clients.length > 0) {
        return clients[0].focus().then((client) => {
          if (message) client.postMessage(message);
        });
      }
      return self.clients.openWindow(SCOPE_URL).then((client) => {
        if (client && message) client.postMessage(message);
      });
    })
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return; // POST/DELETE always hit network

  const url = new URL(req.url);

  // Bypass: anything talking to sessionsd (proxied through vite's /api or
  // /ws) must never be served from cache. Returning early lets the
  // browser handle the request normally.
  if (url.pathname.startsWith('/api/')) return;
  if (url.pathname.startsWith('/ws')) return;

  // Bypass: vite dev-server internals (HMR, /@vite/, /@react-refresh).
  // Caching them would break hot reload while we're working on the app.
  if (url.pathname.startsWith('/@vite/')) return;
  if (url.pathname.startsWith('/@react-refresh')) return;
  if (url.pathname.startsWith('/@id/')) return;
  if (url.pathname.startsWith('/@fs/')) return;

  // Navigation requests (typing the URL, tapping the icon): network-
  // first with cache fallback. The cache update on success keeps
  // /index.html fresh for offline starts.
  if (req.mode === 'navigate') {
    event.respondWith(
      fetch(req)
        .then((res) => {
          const clone = res.clone();
          caches.open(CACHE_VERSION).then((c) => c.put(INDEX_URL, clone)).catch(() => {});
          return res;
        })
        .catch(() =>
          caches.match(INDEX_URL).then((m) => m || new Response('offline', { status: 503 }))
        )
    );
    return;
  }

  // Same-origin assets (JS/CSS/images): cache-first with background
  // refresh. First load fetches + caches; subsequent loads serve from
  // cache instantly while a background fetch updates the entry.
  if (url.origin === self.location.origin) {
    event.respondWith(
      caches.match(req).then((cached) => {
        const fresh = fetch(req)
          .then((res) => {
            if (res && res.status === 200) {
              const clone = res.clone();
              caches.open(CACHE_VERSION).then((c) => c.put(req, clone)).catch(() => {});
            }
            return res;
          })
          // On network failure: return the cached copy if we have one,
          // otherwise return a 503 so respondWith() always gets a valid
          // Response (returning undefined here would throw a network error).
          .catch(() => cached ?? new Response('offline', { status: 503, statusText: 'Service Unavailable' }));
        // Serve cache immediately if available; fall back to the network
        // promise (which itself has the 503 safety net above).
        return cached ?? fresh;
      })
    );
  }
});
