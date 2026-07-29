# Contract fixtures

`runner.json` is a field-complete runner metadata example matching the literal
`SessionMeta` object written by the frozen Node fixture in
`runtime/testdata/node-runtime/src/runner.ts`.

`events.hex.txt` contains three byte-exact `.events` records, including ASCII,
ANSI control bytes, and multi-byte UTF-8.

The `http-*.json` bodies were captured from a freshly built TypeScript daemon
on 2026-07-16. The process was isolated with:

```sh
HOME=/tmp/sessions-contract.E3YvK7/home \
SESSIONS_STATE_DIR=/tmp/sessions-contract.E3YvK7/runners \
SESSIONS_HOST=127.0.0.1 \
SESSIONS_PORT=8899 \
SESSIONS_WEB_DIR=/tmp/sessions-contract.E3YvK7/no-web \
node runtime/testdata/node-runtime/dist/server.js
```

Captured request/status mapping:

| Fixture | Request | Status |
| --- | --- | --- |
| `http-health.json` | `GET /api/health` | 200 |
| `http-health-deep.json` | `GET /api/health/deep` | 200 |
| `http-unauthorized.json` | unauthenticated `GET /api/sessions` | 401 |
| `http-sessions-empty.json` | Bearer-authenticated `GET /api/sessions` | 200 |

The observed common headers were `Content-Type: application/json`, `Vary:
Origin`, `Access-Control-Allow-Methods: GET,POST,DELETE,OPTIONS`, and
`Access-Control-Allow-Headers: content-type, authorization`. An isolated
preflight from `https://sessions.somewhere.tech` (or its canonical
`https://sessions.somewhere.site` redirect target) returned 204 and echoed that
value in `Access-Control-Allow-Origin`; a health request from
`https://evil.example` still returned 200 but had no ACAO header.

`uptimeSec` is intentionally a captured value rather than a stable golden
constant. The scratch token and state were created only below the two `/tmp`
paths above; the daemon was terminated with SIGINT after capture.
