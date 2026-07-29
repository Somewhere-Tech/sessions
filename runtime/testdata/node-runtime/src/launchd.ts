// macOS launchd integration: register each session-runner as a per-user
// LaunchAgent so it survives logout / reboot.
//
// Why per-session plists (vs one global agent that supervises children):
//   - launchd handles RunAtLoad, env, working dir, and stdout/stderr
//     redirection for free.
//   - Killing a session means just `launchctl bootout` + unlink — no
//     bookkeeping in a separate supervisor.
//   - On reboot, launchd auto-starts every plist in
//     ~/Library/LaunchAgents/, the runner re-binds its Unix socket, and
//     prettyd's discoverRunners() reattaches the next time prettyd
//     starts. Zero coordination required.
//
// We deliberately set KeepAlive=false. If the PTY exits naturally we do
// NOT want launchd to relaunch the runner; the session is over. prettyd's
// EXIT handler unloads the plist so it doesn't auto-start on next reboot.

import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawnSync } from 'node:child_process';

export const LAUNCH_AGENTS_DIR = path.join(os.homedir(), 'Library', 'LaunchAgents');
const LABEL_PREFIX = 'tech.pretty-pty.runner.';

export interface PlistArgs {
  id: string;
  programArguments: string[];
  env: Record<string, string>;
  cwd: string;
  logPath: string;
}

function escapeXml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function plistXml(args: PlistArgs): string {
  const envEntries = Object.entries(args.env)
    .map(([k, v]) => `    <key>${escapeXml(k)}</key>\n    <string>${escapeXml(v)}</string>`)
    .join('\n');
  const progArgs = args.programArguments
    .map((a) => `    <string>${escapeXml(a)}</string>`)
    .join('\n');
  return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LABEL_PREFIX}${escapeXml(args.id)}</string>
  <key>ProgramArguments</key>
  <array>
${progArgs}
  </array>
  <key>EnvironmentVariables</key>
  <dict>
${envEntries}
  </dict>
  <key>WorkingDirectory</key>
  <string>${escapeXml(args.cwd)}</string>
  <key>RunAtLoad</key>
  <true/>
  <!-- Restart the runner if it dies UNEXPECTEDLY (crash, kill -9,
       prettyd-side socket cleanup nudging it out), but NOT when the
       underlying PTY closes normally (user typed exit, child exited
       cleanly). SuccessfulExit=false = "respawn only if exit code
       was non-zero or signal-killed."
       Without this, a tsx-watch reload of prettyd would let the
       runners die and stay dead; on next prettyd start discoverRunners
       would find dead sockets and bootout their plists entirely. -->
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <!-- These runners own interactive PTYs. Mark them Interactive so macOS
       does not apply Darwin-background CPU/I/O throttling to keystrokes,
       terminal repaint, or tool startup. -->
  <key>ProcessType</key>
  <string>Interactive</string>
  <key>StandardOutPath</key>
  <string>${escapeXml(args.logPath)}</string>
  <key>StandardErrorPath</key>
  <string>${escapeXml(args.logPath)}</string>
</dict>
</plist>
`;
}

export function plistPathFor(id: string): string {
  return path.join(LAUNCH_AGENTS_DIR, LABEL_PREFIX + id + '.plist');
}

export function labelFor(id: string): string {
  return LABEL_PREFIX + id;
}

function uid(): number {
  return process.getuid?.() ?? 0;
}

// Returns true on success, false (and logs to stderr) on failure.
export function bootstrapRunner(args: PlistArgs): boolean {
  fs.mkdirSync(LAUNCH_AGENTS_DIR, { recursive: true });
  const plistPath = plistPathFor(args.id);
  // 0600: the plist embeds the runner's environment, which includes
  // ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN and proxy/CA settings. Don't
  // rely on umask to keep those owner-only — set the mode explicitly on
  // write, and chmod after in case the file pre-existed with a wider mode.
  fs.writeFileSync(plistPath, plistXml(args), { mode: 0o600 });
  try { fs.chmodSync(plistPath, 0o600); } catch { /* best effort */ }
  // bootstrap loads the plist into the gui/<uid> domain. RunAtLoad fires
  // synchronously inside this call — by the time we return, launchd has
  // started (or attempted to start) the program.
  const r = spawnSync('launchctl', ['bootstrap', `gui/${uid()}`, plistPath], {
    stdio: ['ignore', 'pipe', 'pipe']
  });
  if (r.status !== 0) {
    const err = r.stderr.toString().trim();
    // 17 = "service already loaded" — benign on prettyd reload after a
    // session was already created. Treat as success.
    if (r.status === 17 || /already (loaded|bootstrapped)/i.test(err)) return true;
    console.error(`launchctl bootstrap ${args.id} failed (status=${r.status}): ${err}`);
    return false;
  }
  return true;
}

export function bootoutRunner(id: string): void {
  const plistPath = plistPathFor(id);
  // bootout removes the service from the domain and stops the process
  // if it's still running.
  spawnSync('launchctl', ['bootout', `gui/${uid()}/${labelFor(id)}`], { stdio: 'ignore' });
  try { fs.unlinkSync(plistPath); } catch { /* not present is fine */ }
}

// Drop plists whose session is **truly gone** — meaning even the
// persistent event log has been deleted. Two states get distinguished:
//
//   * .sock + .json missing, .events PRESENT
//       The runner went down (crash, reboot, launchctl bootout, hot
//       reload — anything where the runner exited via SIGTERM with
//       sessionEnded=false). The session is meant to come back; the
//       plist has RunAtLoad and the events file holds the buffer
//       history. Don't touch the plist — let launchd revive it.
//
//   * .sock + .json + .events all gone
//       The runner exited cleanly with sessionEnded=true (user did
//       `pretty kill`, or the program inside the PTY exited). The
//       session is over; clean the plist so launchd won't relaunch
//       it on next reboot.
//
// The earlier version of this function only checked sock+json, which
// meant a SIGTERM-via-launchctl-bootout (a routine reboot path!)
// looked like an orphan to prettyd, and the plist got nuked the next
// time prettyd started — silently destroying the session. That's how
// I lost the original Fit Furniture state, and the bug needs to die
// here.
export function cleanupOrphanPlists(stateDir: string): void {
  if (!fs.existsSync(LAUNCH_AGENTS_DIR)) return;
  const entries = fs.readdirSync(LAUNCH_AGENTS_DIR);
  // How long after a plist is written before we consider it "started" and
  // eligible for orphan checks. A freshly-bootstrapped runner has RunAtLoad
  // but hasn't written any state files yet; without this guard it looks like
  // an orphan during the window between `launchctl bootstrap` and the runner
  // process creating its .json / .sock / .events files. 30s is generous even
  // for the slow tsx cold-start path (which can take up to 60s but always
  // creates the .json meta early in the startup sequence).
  const STARTING_GRACE_MS = 30_000;
  for (const name of entries) {
    if (!name.startsWith(LABEL_PREFIX) || !name.endsWith('.plist')) continue;
    const id = name.slice(LABEL_PREFIX.length, -'.plist'.length);
    const plistPath = path.join(LAUNCH_AGENTS_DIR, name);
    const sockPath = path.join(stateDir, id + '.sock');
    const metaPath = path.join(stateDir, id + '.json');
    const eventsPath = path.join(stateDir, id + '.events');
    if (fs.existsSync(eventsPath)) continue; // session data alive, keep plist
    // Skip plists that were written very recently — the runner may still be
    // booting and hasn't created its state files yet. Being wrong here is
    // catastrophic (destroys a live session); being conservative just delays
    // cleanup by one prettyd restart, which is fine.
    try {
      const plistMtime = fs.statSync(plistPath).mtimeMs;
      if (Date.now() - plistMtime < STARTING_GRACE_MS) continue;
    } catch { continue; } // can't stat the plist — conservative, skip
    if (!fs.existsSync(sockPath) && !fs.existsSync(metaPath)) {
      bootoutRunner(id);
    }
  }
}
