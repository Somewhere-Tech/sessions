import fs from 'node:fs';

const main = fs.readFileSync(new URL('../src/main.tsx', import.meta.url), 'utf8');
const css = fs.readFileSync(new URL('../src/styles/globals.css', import.meta.url), 'utf8');
const native = fs.readFileSync(new URL('../../src-tauri/src/lib.rs', import.meta.url), 'utf8');
const lifecycle = fs.readFileSync(new URL('../../src-tauri/src/lifecycle.rs', import.meta.url), 'utf8');
const connect = fs.readFileSync(new URL('../src/components/ConnectScreen.tsx', import.meta.url), 'utf8');
const sessionsStore = fs.readFileSync(new URL('../src/store/sessions.ts', import.meta.url), 'utf8');

function requireSource(source, pattern, message) {
  if (!pattern.test(source)) throw new Error(message);
}

requireSource(main, /root\.render\(<StartupShell\s*\/>\);\s*void bootstrap\(\)/s,
  'the startup shell must mount before asynchronous bootstrap begins');
requireSource(main, /\.finally\(renderApp\)/,
  'bootstrap success and failure must both render the app');
requireSource(main, /class AppBoundary extends React\.Component/,
  'the root app needs a render error boundary');
requireSource(main, /Your background service and\s*agent sessions were not stopped/s,
  'the recovery view must state that agents remain running');
requireSource(main, /window\.location\.reload\(\)/,
  'the startup and crash states need an in-place reload action');
requireSource(css, /\.startup-shell\s*\{/,
  'the startup shell must have a visible full-window style');
requireSource(native, /let runtime_status = lifecycle::startup_status\(\);[\s\S]*thread::spawn\(move \|\| \{[\s\S]*lifecycle::install_for_app\(&worker\)/,
  'native setup must publish a first-frame status before background runtime reconciliation');
requireSource(lifecycle, /agent sessions keep running/,
  'runtime reconciliation status must explain that agents keep running');
requireSource(connect, /Recovering your sessions/,
  'daemon reconciliation needs a visible, non-alarming recovery state');
requireSource(connect, /messages stay disabled and drafts are never sent until the connection returns/,
  'recovery copy must state honest message-delivery behavior');
requireSource(connect, /Quitting this window does not stop agents or erase session history/,
  'recovery needs a safe exit whose lifecycle semantics are explicit');
requireSource(connect, /will not be mixed with sessions from another computer/,
  'remote-machine recovery must explain that cached rows stay machine-scoped');
requireSource(sessionsStore, /serverId: string \| null/,
  'the session cache must record which machine produced its rows');
requireSource(sessionsStore, /if \(get\(\)\.serverId !== serverId\) return/,
  'in-flight session refreshes must not cross machine scopes');

for (const forbidden of ['killSession(', 'endSession(', 'RequestKill(', '/api/sessions/end']) {
  if (main.includes(forbidden)) {
    throw new Error(`viewer recovery must not own runtime lifecycle: found ${forbidden}`);
  }
}

console.log('startup recovery smoke passed');
