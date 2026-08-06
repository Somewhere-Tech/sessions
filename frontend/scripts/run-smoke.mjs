// The smoke gate: run every frontend smoke suite, and make a failure name
// itself.
//
// This replaces a 24-link `npm run a && npm run b && …` chain in package.json.
// That chain had three properties a gate must not have:
//
//   1. when a suite failed, the useful line ("which suite") was buried above
//      npm's own two-page error epilogue, so the thing everyone actually read
//      was the exit code;
//   2. a suite that HUNG rather than failed was unbounded — it burned the job
//      timeout and surfaced as an unexplained cancellation;
//   3. adding a `test:*` script to package.json without also editing the chain
//      silently left it out of the gate. `test:webapp` is out of the gate right
//      now, and nothing said so.
//
// So: sequential (unchanged semantics), streamed live (nothing is discarded),
// individually time-boxed, and reconciled against package.json so a new suite
// cannot be forgotten.
//
// Usage:
//   node scripts/run-smoke.mjs                  # the gate
//   node scripts/run-smoke.mjs surface-truth …  # a subset, same reporting
//   SMOKE_SUITE_TIMEOUT_MS=60000 …              # tighten the per-suite bound
import { spawn } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

const packagePath = fileURLToPath(new URL('../package.json', import.meta.url));
const frontendDir = fileURLToPath(new URL('..', import.meta.url));

// The gate, in order. Names are the `test:<name>` scripts in package.json; the
// command each one runs is read from there, so this list cannot drift into
// naming a script that does not exist.
const GATE = [
  'markdown',
  'native-credentials',
  'structured-events',
  'structured-view',
  'fleet-clarity',
  'fleet-usage',
  'search-conversations',
  'terminal-renderer',
  'external-links',
  'lifecycle-clarity',
  'workspace-ux',
  'session-status',
  'surface-truth',
  'search-rollup',
  'conversation-browser',
  'resume-partial',
  'shared-contracts',
  'working-set',
  'typography',
  'window-scope',
  'same-origin-bootstrap',
  'startup-recovery',
  'fork-conversation',
  'onboarding-consent'
];

// Scripts that are deliberately not part of the gate, each with the reason it
// cannot run here. Anything in package.json that is neither in GATE nor here is
// a mistake, and this runner refuses to start until it is one or the other.
const NOT_IN_GATE = {
  smoke: 'this runner',
  webapp: 'acceptance check: needs a live daemon and a served build '
    + '(SESSIONS_WEBAPP_URL / SESSIONS_ENDPOINT), so it cannot run unattended here'
};

// Two bounds guard a hung suite, and the order matters. lib/smoke.mjs arms an
// in-process watchdog at SMOKE_SUITE_TIMEOUT_MS; it knows which scenario the
// suite was in, so it must get to speak first. This external bound is the
// backstop for the cases the in-process one cannot cover — a suite that never
// reached `smoke()`, or one wedged somewhere the event loop cannot run — so it
// is deliberately a little later, and it kills the whole process group.
const SUITE_WATCHDOG_MS = Number(process.env.SMOKE_SUITE_TIMEOUT_MS ?? 180_000);
const SUITE_TIMEOUT_MS = SUITE_WATCHDOG_MS + 20_000;
const TOTAL_TIMEOUT_MS = Number(process.env.SMOKE_TOTAL_TIMEOUT_MS ?? 20 * 60_000);
const TAIL_LINES = Number(process.env.SMOKE_TAIL_LINES ?? 40);

const manifest = JSON.parse(await readFile(packagePath, 'utf8'));
const scripts = manifest.scripts ?? {};

// ── Reconcile the gate against package.json before running anything ─────────
const declared = Object.keys(scripts)
  .filter((name) => name.startsWith('test:'))
  .map((name) => name.slice('test:'.length));
const unaccounted = declared.filter((name) => !GATE.includes(name) && !(name in NOT_IN_GATE));
if (unaccounted.length > 0) {
  process.stderr.write(
    `run-smoke: these package.json scripts are in neither the gate nor the documented exclusions:\n`
    + unaccounted.map((name) => `  test:${name}\n`).join('')
    + `Add each to GATE in scripts/run-smoke.mjs, or to NOT_IN_GATE with the reason it cannot run.\n`
  );
  process.exit(2);
}
const missing = GATE.filter((name) => !scripts[`test:${name}`]);
if (missing.length > 0) {
  process.stderr.write(`run-smoke: gate names scripts that package.json does not define: ${missing.join(', ')}\n`);
  process.exit(2);
}

const requested = process.argv.slice(2).filter((argument) => !argument.startsWith('-'));
const unknown = requested.filter((name) => !GATE.includes(name));
if (unknown.length > 0) {
  process.stderr.write(`run-smoke: unknown suite(s): ${unknown.join(', ')}\nknown: ${GATE.join(', ')}\n`);
  process.exit(2);
}
const plan = requested.length > 0 ? requested : GATE;

/** `node scripts/foo.mjs` → ['scripts/foo.mjs']. Anything else is unsupported. */
function argvFor(name) {
  const command = scripts[`test:${name}`];
  const match = /^node\s+(\S+)\s*$/.exec(command.trim());
  if (!match) {
    process.stderr.write(
      `run-smoke: test:${name} is "${command}"; this runner only knows how to run a bare `
      + `\`node <script>\`. Wrap the extra work inside the script itself.\n`
    );
    process.exit(2);
  }
  return match[1];
}

function killTree(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  if (process.platform === 'win32') {
    // Puppeteer leaves a browser under the suite; /T takes the whole tree.
    spawn('taskkill', ['/pid', String(child.pid), '/T', '/F'], { stdio: 'ignore' });
    return;
  }
  try { process.kill(-child.pid, 'SIGTERM'); } catch { /* already gone */ }
  setTimeout(() => {
    try { process.kill(-child.pid, 'SIGKILL'); } catch { /* already gone */ }
  }, 5_000).unref();
}

/**
 * Run one suite. Output is streamed straight through — nothing this runner does
 * may cause a failure's own explanation to be discarded — and the tail is kept
 * so the report can repeat it under the banner, where people look.
 */
function runSuite(name) {
  const script = argvFor(name);
  return new Promise((resolve) => {
    const startedAt = Date.now();
    const child = spawn(process.execPath, [script], {
      cwd: frontendDir,
      stdio: ['ignore', 'pipe', 'pipe'],
      detached: process.platform !== 'win32',
      env: { ...process.env, SMOKE_SUITE: name }
    });

    const tail = [];
    const keep = (chunk, sink) => {
      sink.write(chunk);
      for (const line of chunk.toString().split('\n')) {
        tail.push(line);
        if (tail.length > TAIL_LINES) tail.shift();
      }
    };
    child.stdout.on('data', (chunk) => keep(chunk, process.stdout));
    child.stderr.on('data', (chunk) => keep(chunk, process.stderr));

    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      killTree(child);
    }, SUITE_TIMEOUT_MS);

    child.on('error', (error) => {
      clearTimeout(timer);
      resolve({ name, script, ok: false, timedOut: false, reason: error.message, ms: Date.now() - startedAt, tail });
    });
    child.on('close', (code, signal) => {
      clearTimeout(timer);
      const ms = Date.now() - startedAt;
      resolve({
        name,
        script,
        ok: !timedOut && code === 0,
        timedOut,
        code,
        signal,
        reason: timedOut
          ? `no output-producing exit within ${SUITE_TIMEOUT_MS} ms — the suite HUNG rather than failed`
          : signal
            ? `killed by ${signal}`
            : `exited ${code}`,
        ms,
        tail
      });
    });
  });
}

const overall = setTimeout(() => {
  process.stderr.write(`\nrun-smoke: the whole gate exceeded ${TOTAL_TIMEOUT_MS} ms; aborting.\n`);
  process.exit(1);
}, TOTAL_TIMEOUT_MS);
overall.unref();

const started = Date.now();
const results = [];
let failure = null;

for (const name of plan) {
  process.stdout.write(`\n── test:${name} ${'─'.repeat(Math.max(1, 60 - name.length))}\n`);
  const result = await runSuite(name);
  results.push(result);
  if (!result.ok) { failure = result; break; }
}

clearTimeout(overall);
const elapsed = ((Date.now() - started) / 1000).toFixed(1);

process.stdout.write('\n');
for (const result of results) {
  process.stdout.write(
    `  ${result.ok ? 'pass' : 'FAIL'}  ${String((result.ms / 1000).toFixed(1)).padStart(6)}s  test:${result.name}\n`
  );
}

if (!failure) {
  process.stdout.write(`\nsmoke gate passed: ${results.length} suites in ${elapsed}s\n`);
  process.exit(0);
}

// The whole point of this file.
const bar = '═'.repeat(72);
process.stderr.write([
  '',
  bar,
  `  SMOKE GATE FAILED`,
  '',
  `  suite:      test:${failure.name}`,
  `  script:     frontend/${failure.script}`,
  `  outcome:    ${failure.reason}`,
  `  ran for:    ${(failure.ms / 1000).toFixed(1)}s`,
  `  reproduce:  npm --prefix frontend run test:${failure.name}`,
  failure.timedOut
    ? '\n  A hang is not a slow machine. The suite held the event loop past its\n'
      + '  budget without reaching a pass line or throwing. The scenario it was\n'
      + '  waiting in is in the tail below; if the tail is empty, it never got\n'
      + '  past start-up.'
    : '',
  '',
  `  last ${TAIL_LINES} lines of its output:`,
  bar,
  ...failure.tail.map((line) => `  ${line}`),
  bar,
  `  ${results.length - 1} suite(s) passed before it; ${GATE.length - results.length} were not reached.`,
  bar,
  ''
].join('\n'));
process.exit(1);
