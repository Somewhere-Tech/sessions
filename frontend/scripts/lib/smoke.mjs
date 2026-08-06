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
import { inspect } from 'node:util';

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
      }),
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
