// Shared vocabulary for the smoke suites, so a failure explains itself.
//
// The gate is ~24 sequential node scripts. When one of them fails on a loaded
// CI machine the only thing anyone reads is the last screenful of output, and
// what that screenful used to say was:
//
//     Waiting for selector `.app-shell` failed: 10000ms exceeded
//
// which does not say which suite, which scenario, or what the screen actually
// looked like at the moment it gave up. That is the difference between "the
// runner was starved for ten seconds" and "the app crashed on boot and never
// mounted", and re-running is the only way to tell them apart. Re-running is
// exactly the habit a gate must not teach.
//
// This module gives every wait a sentence and every timeout a snapshot of the
// page it was waiting on. It changes no assertion and relaxes no timeout.
//
// It also removes the one shared mutable input the gate had — see
// stableDistSnapshot, which is where the reproducible flake actually lived.
import { cp, mkdtemp, readFile, rm, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { inspect } from 'node:util';

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

/**
 * A deadline for a teardown step that must not become a hang.
 *
 * The obvious spelling — `Promise.race([close(), delay(5_000)])` — is a trap:
 * when `close()` wins, the timer is still pending and still referenced, so node
 * cannot exit until it fires. That silently adds the full deadline to every
 * *passing* run. It was costing this gate five seconds per suite it was used
 * in. Unref the timer: it still fires if the loop is otherwise alive (which it
 * is, when something is genuinely wedged) and never delays a clean exit.
 */
function withDeadline(promise, milliseconds) {
  return Promise.race([
    promise,
    new Promise((resolve) => {
      const timer = setTimeout(resolve, milliseconds);
      timer.unref?.();
    })
  ]);
}

/**
 * Close a puppeteer browser without ever waiting forever for it. A wedged
 * Chromium must not turn a finished suite into a silent hang.
 */
export async function closeBrowser(browser, milliseconds = 5_000) {
  if (!browser) return;
  const child = browser.process();
  await withDeadline(browser.close().catch(() => {}), milliseconds);
  if (child && child.exitCode === null && child.signalCode === null) {
    child.kill('SIGKILL');
  }
}

/**
 * Close an http.Server without waiting on sockets the browser may not have
 * released. `server.close()` alone does not return until every connection is
 * gone, which is how a passing suite became a hang.
 */
export async function closeServer(server, milliseconds = 5_000) {
  if (!server) return;
  server.closeAllConnections?.();
  await withDeadline(new Promise((resolve) => server.close(resolve)), milliseconds);
}

/**
 * An immutable copy of `frontend/dist` for a suite to serve.
 *
 * Three suites (window-scope, same-origin-bootstrap, native-credentials) serve
 * the real build over a static server and drive it with puppeteer. They used to
 * serve `frontend/dist` directly, which makes a shared, mutable directory an
 * input to the gate — and `vite build` *empties* dist before it rewrites it.
 * So any concurrent build on the machine (another agent, a watch task, a second
 * checkout's `npm run build`) pulls the app out from under a running suite.
 *
 * This is reproducible and it is not subtle. Running `vite build` in a loop
 * beside these three suites failed 4 of ~9 suite runs, in two shapes:
 *
 *   - dist gone at start-up: "frontend/dist is missing" (at least legible);
 *   - dist emptied mid-run: every asset request 404s, the app never mounts,
 *     and the suite dies on a selector wait — which reads exactly like a slow
 *     machine and is why re-running "fixes" it.
 *
 * A copy costs ~1.5 MB and a few milliseconds, and removes the shared input
 * entirely. It asserts nothing less than before: the same real build, read from
 * somewhere nobody else is writing.
 *
 * The copy itself can catch dist mid-rewrite, so the snapshot is verified
 * complete — index.html plus every local asset it references — and retried.
 */
export async function stableDistSnapshot(label, { attempts = 6 } = {}) {
  const dist = fileURLToPath(new URL('../../dist/', import.meta.url));
  let problem = 'unknown';
  for (let attempt = 1; attempt <= attempts; attempt += 1) {
    let target;
    try {
      await stat(join(dist, 'index.html'));
      target = await mkdtemp(join(tmpdir(), `sessions-dist-${label}-`));
      await cp(dist, target, { recursive: true });
      const html = await readFile(join(target, 'index.html'), 'utf8');
      const referenced = [...html.matchAll(/(?:src|href)="(\/[^"]+)"/g)].map((match) => match[1]);
      const missing = [];
      for (const reference of referenced) {
        try {
          await stat(join(target, reference.replace(/^\/+/, '')));
        } catch {
          missing.push(reference);
        }
      }
      if (missing.length === 0) return target;
      problem = `index.html referenced assets that were not in dist: ${missing.join(', ')}`;
    } catch (error) {
      problem = error.message;
    }
    if (target) await rm(target, { recursive: true, force: true });
    // A build is mid-flight. Wait for it to finish rather than reading a
    // half-written tree; the retry budget below is what turns "someone is
    // building forever" into a failure with a reason.
    await delay(1_000);
  }
  throw new Error(
    `${label}: could not take a consistent snapshot of frontend/dist after ${attempts} attempts.\n`
    + `Last problem: ${problem}\n`
    + 'Either the build has not been run (npm --prefix frontend run build), or something '
    + 'is rewriting frontend/dist continuously while this suite tries to read it.'
  );
}

const isTimeout = (error) => Boolean(error)
  && (error.name === 'TimeoutError' || /timed? ?out|exceeded/i.test(error.message ?? ''));

/**
 * Puppeteer buries the budget it actually used in the innermost cause
 * ("Waiting failed: 15000ms exceeded"). Report the real number, so nobody has
 * to guess whether a wait was given ten seconds or one.
 */
function elapsedFrom(error) {
  for (let current = error, depth = 0; current && depth < 6; current = current.cause, depth += 1) {
    const match = /(\d+)ms exceeded/.exec(current.message ?? '');
    if (match) return match[1];
  }
  return null;
}

/**
 * A short description of what is actually on the page, so a timeout can be
 * read without re-running. The point is to separate the two failures that look
 * identical from the outside:
 *
 *   - `mounted: false` and no landmarks — the app never booted (a real bug, or
 *     a bundle that failed to load), and no amount of extra time would help;
 *   - landmarks present but the awaited node missing — the app is up and the
 *     thing under test genuinely did not happen;
 *   - `pageErrors` non-empty — a thrown exception is the cause, and the wait is
 *     only the messenger.
 */
async function describePage(page) {
  if (!page) return null;
  try {
    const digest = await Promise.race([
      page.evaluate(() => {
        const trim = (value) => (value ?? '').replace(/\s+/g, ' ').trim().slice(0, 200);
        const ids = Array.from(document.querySelectorAll('[id]'), (node) => node.id)
          .filter(Boolean)
          .slice(0, 25);
        return {
          url: location.href,
          readyState: document.readyState,
          mounted: Boolean(document.querySelector('#root')?.firstElementChild)
            || Boolean(document.querySelector('.app-shell')),
          landmarkIds: ids,
          testids: Array.from(
            document.querySelectorAll('[data-testid]'),
            (node) => node.getAttribute('data-testid')
          ).slice(0, 25),
          bodyText: trim(document.body?.innerText)
        };
      // The race below can finish first, leaving this promise to settle with
      // nobody listening. Swallow it here: an unhandled rejection from the
      // diagnostic would replace the real failure report with its own.
      }).catch((error) => ({ note: `page digest failed: ${error.message}` })),
      new Promise((resolve) => setTimeout(() => resolve({ note: 'page did not answer the digest probe' }), 3_000))
    ]);
    return digest;
  } catch (error) {
    return { note: `page digest unavailable: ${error.message}` };
  }
}

class Suite {
  constructor(name) {
    this.name = name;
    this.currentScenario = null;
    this.startedAt = Date.now();
    this.pages = new Set();
    this.finished = false;

    // A suite that hangs instead of failing is the worst outcome: it burns the
    // whole job budget and surfaces as an unexplained cancellation in somebody
    // else's run. The runner enforces this from the outside too; this is the
    // in-process copy that still knows the scenario name.
    const budget = Number(process.env.SMOKE_SUITE_TIMEOUT_MS ?? 180_000);
    this.watchdog = setTimeout(() => {
      process.stderr.write(this.banner(
        `exceeded its ${budget} ms budget without finishing`,
        'The suite is stuck, not slow: nothing threw, it simply never reached its\n'
        + 'pass line. Whatever the scenario below was waiting on never happened.'
      ));
      process.exit(1);
    }, budget);
    // Deliberately unref'd: this must never be the reason a healthy suite keeps
    // running. A hung suite is by definition still holding handles (a browser
    // pipe, a listening server), so the loop is alive and this still fires. The
    // authoritative bound is the runner's external one in run-smoke.mjs, which
    // also kills the orphaned browser this exit path would otherwise leave.
    this.watchdog.unref?.();

    process.on('unhandledRejection', (reason) => {
      const error = reason instanceof Error ? reason : new Error(inspect(reason));
      process.stderr.write(this.banner('failed on an unhandled rejection', error.stack ?? error.message));
      process.exit(1);
    });
  }

  banner(headline, detail) {
    const elapsed = ((Date.now() - this.startedAt) / 1000).toFixed(1);
    const lines = [
      '',
      `── ${this.name} smoke FAILED ${'─'.repeat(Math.max(0, 56 - this.name.length))}`,
      `   suite:    ${this.name}`,
      `   scenario: ${this.currentScenario ?? '(none declared yet — failed before the first scenario)'}`,
      `   after:    ${elapsed}s`,
      `   ${headline}`,
      ''
    ];
    if (detail) lines.push(String(detail).split('\n').map((line) => `   ${line}`).join('\n'), '');
    return lines.join('\n');
  }

  /**
   * Name the thing being proved. Costs nothing when the suite passes; it is
   * the first line of the report when it does not.
   */
  scenario(label) {
    this.currentScenario = label;
    if (process.env.SMOKE_VERBOSE) process.stdout.write(`  · ${this.name}: ${label}\n`);
    return label;
  }

  /** Register a page so a timeout can describe what was on screen. */
  watch(page) {
    if (page) this.pages.add(page);
    return page;
  }

  async #rethrow(error, { what, page, timeout, locator }) {
    if (!isTimeout(error)) throw error;
    const digest = await describePage(page ?? [...this.pages].at(-1));
    const detail = [
      `waited ${timeout ?? elapsedFrom(error) ?? 'the page default'} ms for ${what}`,
      locator ? `locator:  ${locator}` : null,
      '',
      'what the page looked like when it gave up:',
      inspect(digest, { depth: 4, breakLength: 100 })
    ].filter((line) => line !== null).join('\n');
    const wrapped = new Error(
      `${this.name} › ${this.currentScenario ?? 'no scenario'}: timed out waiting for ${what}`
    );
    wrapped.cause = error;
    // Non-enumerable: the banner above is where a human reads this. Attaching
    // it enumerably makes node re-print the whole digest inside the stack dump.
    Object.defineProperty(wrapped, 'smokeDetail', { value: detail, enumerable: false });
    process.stderr.write(this.banner(`timed out waiting for ${what}`, detail));
    throw wrapped;
  }

  /**
   * `page.waitForSelector`, plus a sentence saying what the selector meant.
   * The timeout value is passed straight through — this wraps the failure, it
   * does not extend the wait.
   */
  async waitForSelector(page, selector, what, options) {
    this.watch(page);
    try {
      return await page.waitForSelector(selector, options);
    } catch (error) {
      return await this.#rethrow(error, {
        what, page, locator: selector, timeout: options?.timeout
      });
    }
  }

  /** `page.waitForFunction`, with the predicate's intent spelled out. */
  async waitForFunction(page, predicate, what, options, ...args) {
    this.watch(page);
    try {
      return await page.waitForFunction(predicate, options, ...args);
    } catch (error) {
      return await this.#rethrow(error, {
        what,
        page,
        locator: String(predicate).replace(/\s+/g, ' ').slice(0, 240),
        timeout: options?.timeout
      });
    }
  }

  /** `page.waitForResponse` / any awaited puppeteer promise, named. */
  async waitFor(promiseOrThunk, what, { page, timeout } = {}) {
    this.watch(page);
    try {
      return await (typeof promiseOrThunk === 'function' ? promiseOrThunk() : promiseOrThunk);
    } catch (error) {
      return await this.#rethrow(error, { what, page, timeout });
    }
  }

  /**
   * A hard bound for anything that is not a puppeteer wait — a bare promise, a
   * server that must bind, a child process that must exit. Without this an
   * `await` on something that never settles is indistinguishable from a slow
   * machine until the job timeout kills the run.
   */
  async bounded(promise, what, milliseconds = 30_000) {
    let timer;
    try {
      return await Promise.race([
        promise,
        new Promise((_, reject) => {
          timer = setTimeout(
            () => reject(Object.assign(new Error(`timed out after ${milliseconds}ms`), { name: 'TimeoutError' })),
            milliseconds
          );
        })
      ]);
    } catch (error) {
      if (!isTimeout(error)) throw error;
      process.stderr.write(this.banner(`timed out after ${milliseconds} ms waiting for ${what}`));
      throw new Error(`${this.name} › ${this.currentScenario ?? 'no scenario'}: ${what} did not complete within ${milliseconds}ms`);
    } finally {
      clearTimeout(timer);
    }
  }

  /** The suite's single success line. Also releases the watchdog. */
  pass(message) {
    this.finished = true;
    clearTimeout(this.watchdog);
    process.stdout.write(`${message ?? `${this.name} smoke passed`}\n`);
  }

  /** Release the watchdog without claiming success (use in `finally`). */
  release() {
    clearTimeout(this.watchdog);
  }
}

export function smoke(name) {
  return new Suite(name);
}
