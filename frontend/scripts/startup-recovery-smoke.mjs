import fs from 'node:fs';

const main = fs.readFileSync(new URL('../src/main.tsx', import.meta.url), 'utf8');
const css = fs.readFileSync(new URL('../src/styles/globals.css', import.meta.url), 'utf8');
const native = fs.readFileSync(new URL('../../src-tauri/src/lib.rs', import.meta.url), 'utf8');
const lifecycle = fs.readFileSync(new URL('../../src-tauri/src/lifecycle.rs', import.meta.url), 'utf8');
const app = fs.readFileSync(new URL('../src/App.tsx', import.meta.url), 'utf8');
const recovery = fs.readFileSync(new URL('../src/components/MachineRecoveryNotice.tsx', import.meta.url), 'utf8');
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
// The ordering is the contract, not the exact expression: setup must resolve
// and publish a startup status before spawning background reconciliation. The
// pattern used to pin `let runtime_status = lifecycle::startup_status();`
// literally and went stale the moment the value was wrapped
// (`resolved_port.annotate(...)`), asserting nothing about the property it
// exists to protect.
requireSource(native, /let runtime_status = [^;]*lifecycle::startup_status\(\)[^;]*;[\s\S]*thread::spawn\(move \|\| \{[\s\S]*lifecycle::install_for_app\(&worker\)/,
  'native setup must publish a first-frame status before background runtime reconciliation');
requireSource(lifecycle, /agent sessions keep running/,
  'runtime reconciliation status must explain that agents keep running');
requireSource(app, /<MachineRecoveryNotice/,
  'daemon reconciliation must stay inside the normal workspace');
requireSource(recovery, /Agent processes keep running separately from this window/,
  'recovery copy must preserve runner and viewer lifecycle semantics');
requireSource(recovery, /Showing this machine’s last-known sessions/,
  'remote-machine recovery must keep machine-scoped history visible');
requireSource(recovery, /Start on \{localAlternative\}/,
  'an unavailable remote machine should offer an explicit fresh local start');
if (/sessionsError && !sessionsHydrated[\s\S]{0,500}<ConnectScreen/.test(app)) {
  throw new Error('runtime recovery must not replace the application with ConnectScreen');
}
requireSource(sessionsStore, /serverId: string \| null/,
  'the session cache must record which machine produced its rows');
requireSource(sessionsStore, /machines: Record<string, CachedSessionMachine>/,
  'the cache must retain independent last-known rows for every configured machine');
requireSource(sessionsStore, /if \(get\(\)\.serverId !== serverId\) return/,
  'in-flight session refreshes must not cross machine scopes');

for (const forbidden of ['killSession(', 'endSession(', 'RequestKill(', '/api/sessions/end']) {
  if (main.includes(forbidden)) {
    throw new Error(`viewer recovery must not own runtime lifecycle: found ${forbidden}`);
  }
}

console.log('startup recovery smoke passed');
