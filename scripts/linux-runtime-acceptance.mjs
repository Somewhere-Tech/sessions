import assert from 'node:assert/strict';
import { spawn, execFile } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtemp, mkdir, open, readFile, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { resolve, join } from 'node:path';
import { promisify } from 'node:util';

const run = promisify(execFile);
const delay = (ms) => new Promise((done) => setTimeout(done, ms));
const nativeAMD64 = process.platform === 'linux' && process.arch === 'x64';
// Local fixture validation may exercise the harness on another Unix host;
// its receipt always records that this is NOT native Linux AMD64 acceptance.
const unixFixture = ['darwin', 'linux'].includes(process.platform)
  && process.env.SESSIONS_ACCEPTANCE_FIXTURE === '1';
assert.ok(nativeAMD64 || unixFixture, 'Native Linux AMD64 host or explicitly selected Unix fixture required');
assert.equal(process.argv.length, 4, 'Usage: node scripts/linux-runtime-acceptance.mjs RUNTIME_DIR RECEIPT_JSON');
const runtime = resolve(process.argv[2]);
const receiptPath = resolve(process.argv[3]);
const root = await mkdtemp('/tmp/sl-');
const home = join(root, 'h');
const runners = join(root, 'r');
await mkdir(home);
await mkdir(runners);
const reservation = createServer();
await new Promise((done, fail) => { reservation.once('error', fail); reservation.listen(0, '127.0.0.1', done); });
const port = reservation.address().port;
await new Promise((done) => reservation.close(done));
const env = { ...process.env, HOME: home, SESSIONS_STATE_DIR: runners,
  SESSIONS_LEDGER_PATH: join(root, 'ledger.sqlite3'), SESSIONS_PORT: String(port),
  SHELL: '/bin/bash', PATH: `${runtime}:${process.env.PATH}` };
const report = { source_sha: process.env.SOURCE_SHA || null, platform: process.platform, architecture: process.arch,
  native_linux_amd64: nativeAMD64, status: 'failed', isolation_root: root, binaries: {}, checks: {},
  excluded: ['provider login', 'OS reboot', 'systemd installation', 'published npm release installation'] };
const log = await open(join(root, 'daemon.log'), 'a');
let daemon;
let sessionId;
let runnerPid;

async function preserveDaemonDiagnostics() {
  const source = await open(join(root, 'daemon.log'), 'r');
  try {
    const { size } = await source.stat();
    const bytes = Buffer.alloc(Math.min(size, 16 * 1024));
    await source.read(bytes, 0, bytes.length, Math.max(0, size - bytes.length));
    report.daemon_diagnostics = { pid: daemon?.pid || null, exit_code: daemon?.exitCode ?? null,
      exit_signal: daemon?.signalCode ?? null, configured_http_host: env.SESSIONS_HOST || '127.0.0.1',
      configured_http_port: port, configured_pprof: env.SESSIONS_PPROF || 'default:127.0.0.1:0',
      smoke_mode: env.SESSIONS_SMOKE === '1', log_bytes: size, log_tail_truncated: size > bytes.length,
      log_tail: bytes.toString('utf8') };
    await writeFile(`${receiptPath}.daemon.log`, bytes);
  } finally {
    await source.close();
  }
}

async function eventually(label, check) {
  let last;
  for (let attempt = 0; attempt < 100; attempt++) {
    try { const value = await check(); if (value) return value; } catch (error) { last = error; }
    await delay(100);
  }
  throw new Error(`${label} did not become ready within 10s${last ? `: ${last.message}` : ''}`);
}

async function cli(...args) {
  return (await run(join(runtime, 'sessions'), args, { env, cwd: home, timeout: 10_000, maxBuffer: 1024 * 1024 })).stdout;
}

async function request(headers = {}) {
  const response = await fetch(`http://127.0.0.1:${port}/api/sessions`, { headers, signal: AbortSignal.timeout(1000) });
  return { status: response.status, body: await response.json() };
}

async function startDaemon() {
  daemon = spawn(join(runtime, 'sessionsd'), [], { env, cwd: home, stdio: ['ignore', log.fd, log.fd] });
  let startupError;
  daemon.once('error', (error) => { startupError = error; });
  await eventually('daemon', async () => {
    if (startupError) throw startupError;
    assert.equal(daemon.exitCode, null, 'daemon exited during startup');
    return (await request()).status === 200;
  });
}

async function stopDaemon() {
  if (!daemon || daemon.exitCode !== null || daemon.signalCode !== null) return;
  const exited = new Promise((done) => daemon.once('exit', done));
  daemon.kill('SIGTERM');
  await Promise.race([exited, delay(5000).then(() => { throw new Error('Owned daemon did not stop'); })]);
}

async function processIdentity(pid) {
  assert.ok(Number.isSafeInteger(pid) && pid > 0, 'Expected a real process id');
  process.kill(pid, 0);
  if (process.platform === 'linux') {
    const stat = await readFile(`/proc/${pid}/stat`, 'utf8');
    return stat.slice(stat.lastIndexOf(')') + 2).split(' ')[19]; // /proc starttime (field22)
  }
  return (await run('ps', ['-p', String(pid), '-o', 'lstart='])).stdout.trim();
}

async function runnerRecord() {
  if (process.platform === 'darwin') {
    const label = `gui/${process.getuid()}/tech.somewhere.sessions.runner.${sessionId}`;
    const output = (await run('launchctl', ['print', label], { timeout: 1000 })).stdout;
    return { pid: Number(output.match(/\bpid = (\d+)/)?.[1]) };
  }
  return JSON.parse(await readFile(join(runners, `${sessionId}.launch.json`), 'utf8'));
}

async function communicate(marker) {
  const result = JSON.parse(await cli('send', sessionId, `printf '${marker}\\n'`, '--json'));
  // A PTY receipt honestly remains unconfirmed; prove output independently.
  assert.equal(result.confidence, 'unconfirmed');
  await eventually(marker, async () => (await cli('tail', sessionId)).split(/\r?\n/).includes(marker));
  return { receipt_confidence: result.confidence, literal_output_observed: true };
}

try {
  for (const name of ['sessions', 'sessionsd', 'sessions-runner']) {
    report.binaries[name] = createHash('sha256').update(await readFile(join(runtime, name))).digest('hex');
  }
  await startDaemon();
  const created = JSON.parse(await cli('new', '--tool', 'shell', '--cwd', home, '--name', 'Linux acceptance fixture', '--json'));
  sessionId = created.id;
  assert.equal(created.ok, true);
  assert.equal(created.exited, false);
  assert.equal(created.start_error, undefined);
  const launch = await eventually('runner launch record', async () => {
    const record = await runnerRecord();
    return record.pid > 0 ? record : null;
  });
  runnerPid = launch.pid;
  const runnerIdentity = await processIdentity(launch.pid);
  const childIdentity = await processIdentity(created.pid);
  report.checks.creation = { session_id: sessionId, runner_pid: launch.pid, child_pid: created.pid };
  report.checks.communication = await communicate('LINUX_ACCEPTANCE_BEFORE_RESTART');
  const oldDaemon = daemon.pid;
  await stopDaemon();
  assert.equal(await processIdentity(launch.pid), runnerIdentity, 'Runner changed after daemon exit');
  assert.equal(await processIdentity(created.pid), childIdentity, 'Provider child changed after daemon exit');
  await startDaemon();
  assert.notEqual(daemon.pid, oldDaemon);
  const adopted = await eventually('runner adoption', async () => (await request()).body.sessions.find((item) => item.id === sessionId));
  assert.equal(adopted.pid, created.pid);
  assert.equal(adopted.exited, false);
  assert.equal(await processIdentity(launch.pid), runnerIdentity, 'Daemon restart replaced the runner');
  report.checks.daemon_restart = { preserved_runner_pid: launch.pid, preserved_child_pid: created.pid,
    previous_daemon_pid: oldDaemon, current_daemon_pid: daemon.pid, runner_start_identity_unchanged: true };
  report.checks.communication_after_restart = await communicate('LINUX_ACCEPTANCE_AFTER_RESTART');
  assert.equal((await request()).status, 200, 'Direct loopback authority should work');
  const forwarded = { 'X-Forwarded-For': '203.0.113.1' };
  assert.equal((await request(forwarded)).status, 401);
  assert.equal((await request({ ...forwarded, Authorization: 'Bearer invalid-fixture' })).status, 401);
  const token = (await readFile(join(runners, 'token'), 'utf8')).trim();
  assert.equal((await request({ ...forwarded, Authorization: `Bearer ${token}` })).status, 200);
  report.checks.auth = { direct_loopback: 200, forwarded_missing: 401, forwarded_invalid: 401, forwarded_valid: 200 };
  report.status = 'passed';
} catch (error) {
  report.error = error.message;
  process.exitCode = 1;
} finally {
  try {
    if (sessionId) {
      // Reattach to this completely isolated state if the failing restart left
      // no daemon. Never fall back to the user's service or a broad PID kill.
      if (!daemon?.pid || daemon.exitCode !== null || daemon.signalCode !== null) await startDaemon();
      const ended = JSON.parse(await cli('kill', sessionId, '--reason', 'Finished isolated Linux acceptance', '--json'));
      assert.equal(ended.items.find((item) => item.id === sessionId)?.status, 'killed');
    }
    await stopDaemon();
    report.cleanup = 'owned runtime and daemon stopped; isolated diagnostic state retained';
  } catch (error) {
    report.status = 'failed';
    report.cleanup_error = error.message;
    report.retained_owned_session = { id: sessionId, runner_pid: runnerPid || null,
      next_action: 'Restart a daemon with the four recorded isolation paths, then kill this exact session id; diagnostic state is preserved.' };
    if (daemon?.exitCode === null) daemon.kill('SIGKILL');
    process.exitCode = 1;
  }
  await log.close();
  await mkdir(resolve(receiptPath, '..'), { recursive: true });
  await preserveDaemonDiagnostics();
  await writeFile(receiptPath, `${JSON.stringify(report, null, 2)}\n`);
  console.log(JSON.stringify(report));
}
