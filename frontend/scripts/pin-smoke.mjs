// The pin, end to end: the grouping function that files a pinned session, and
// the mounted navigator a person actually uses to make one.
//
// The second half exists because the first half passed while the feature was
// unreachable. Pinning shipped — daemon, API, CLI, details-panel toggle — and
// the row's ••• menu never offered it, so the only assertions that could have
// caught the report are the ones that mount the real component and read what
// the menu says.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { extname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { smoke, closeBrowser, closeServer } from './lib/smoke.mjs';

const t = smoke('pin');
const scratch = await mkdtemp(join(tmpdir(), 'sessions-pin-smoke-'));
const output = join(scratch, 'working-set.mjs');
const publicDir = fileURLToPath(new URL('../public/', import.meta.url));
let browser;
let server;

try {
  // ── Part 1: the grouping function ────────────────────────────────────────
  t.scenario('lib/workingSet.ts files a pinned session into its own group');
  await build({
    entryPoints: ['src/lib/workingSet.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });

  const {
    groupWorkingSet,
    isPinned,
    pinnedFirst,
    pinnedSessionIds,
    PIN_UNAVAILABLE_WHEN_ENDED
  } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const session = (id, values = {}) => ({
    id,
    exited: false,
    createdAt: 1,
    lastDataAt: 1,
    ...values
  });

  // The pin is the daemon's boolean. A daemon too old to send it must read as
  // "not pinned" rather than as undefined leaking into a filter.
  assert.equal(isPinned(session('pinned', { pinned: true })), true);
  assert.equal(isPinned(session('plain', { pinned: false })), false);
  assert.equal(isPinned(session('old-daemon')), false);

  // The list is what the mark is for: pinned first, everything else in exactly
  // the order it already had.
  const ordered = [
    session('first-unpinned'),
    session('pinned-late', { pinned: true }),
    session('second-unpinned'),
    session('pinned-later', { pinned: true })
  ];
  assert.deepEqual(
    pinnedFirst(ordered).map(({ id }) => id),
    ['pinned-late', 'pinned-later', 'first-unpinned', 'second-unpinned'],
    'pinned sessions must float to the top without reordering either half'
  );
  assert.deepEqual(
    ordered.map(({ id }) => id),
    ['first-unpinned', 'pinned-late', 'second-unpinned', 'pinned-later'],
    'pinnedFirst must not mutate its input'
  );
  assert.deepEqual(pinnedSessionIds(ordered), ['pinned-late', 'pinned-later']);

  // (b) A pinned session LEAVES the group the machinery filed it under. It used
  // to be sorted to the top of that group, which is not a group the user can
  // see; now it is lifted out of Live and out of Quiet into Pinned, and it is
  // in exactly one of them.
  const sessions = [
    session('aside-and-pinned', { setAsideAt: 100, pinned: true }),
    session('aside-only', { setAsideAt: 100 }),
    session('running', {})
  ];
  const grouped = groupWorkingSet(sessions, [], pinnedSessionIds(sessions));
  assert.equal(grouped.pinnedIds.has('aside-and-pinned'), true,
    'a pinned session must land in the Pinned group');
  assert.equal(grouped.runningIds.has('aside-and-pinned'), false,
    'a pinned session must LEAVE the running group rather than sort first inside it');
  assert.equal(grouped.setAsideIds.has('aside-and-pinned'), false,
    'a pinned session must never be swept into Quiet');
  assert.deepEqual(grouped.pinnedRoots.map(({ id }) => id), ['aside-and-pinned']);
  assert.deepEqual(grouped.runningRoots.map(({ id }) => id), ['running']);
  assert.equal(grouped.setAsideIds.has('aside-only'), true,
    'an unpinned set-aside session is untouched by any of this');

  // Exactly once in the tree, counted rather than argued about.
  const everywhere = [
    ...grouped.pinnedIds,
    ...grouped.runningIds,
    ...grouped.setAsideIds,
    ...grouped.ended.map(({ id }) => id)
  ];
  assert.equal(new Set(everywhere).size, everywhere.length,
    'no session may appear in two groups at once');

  // (c) A pinned manager keeps its children. They travel with it into Pinned
  // rather than being stranded in the group their manager left, and the manager
  // is still the only root, so the navigator renders them nested under it.
  const family = [
    session('manager', { pinned: true }),
    session('child', { parentSessionId: 'manager' }),
    session('grandchild', { parentSessionId: 'child' }),
    session('later-child', { parentSessionId: 'manager', setAsideAt: 100 })
  ];
  const nested = groupWorkingSet(family, [], pinnedSessionIds(family));
  assert.deepEqual(nested.pinnedRoots.map(({ id }) => id), ['manager'],
    'a pinned manager is the only root of its subtree in the Pinned group');
  assert.equal(nested.pinnedIds.has('child'), true);
  assert.equal(nested.pinnedIds.has('grandchild'), true,
    'the whole subtree follows its pinned manager, not just the first level');
  assert.equal(nested.runningIds.has('child'), false);
  // The one exception, and it is a mark the user also made by hand: a child
  // explicitly moved to Later stays in Quiet.
  assert.equal(nested.pinnedIds.has('later-child'), false);
  assert.equal(nested.setAsideIds.has('later-child'), true);

  // (d) Unpinning returns the session to the group it came from — the same list
  // and the same call, with the pin removed.
  const unpinned = groupWorkingSet(family, [], []);
  assert.deepEqual(unpinned.pinnedRoots.map(({ id }) => id), []);
  assert.equal(unpinned.pinnedIds.size, 0);
  assert.deepEqual(unpinned.runningRoots.map(({ id }) => id), ['manager'],
    'unpinning puts the manager back where the machinery had it');
  assert.equal(unpinned.runningIds.has('child'), true);
  assert.equal(unpinned.runningIds.has('grandchild'), true);
  assert.equal(unpinned.setAsideIds.has('later-child'), true);

  // An ended conversation the user pinned stays reachable, in the user's own
  // group, rather than dropping into the ended pile.
  const withEnded = [session('ended-pinned', { exited: true, pinned: true })];
  const endedGroups = groupWorkingSet(withEnded, [], pinnedSessionIds(withEnded));
  assert.equal(endedGroups.pinnedIds.has('ended-pinned'), true);
  assert.equal(endedGroups.runningIds.has('ended-pinned'), false);
  assert.equal(endedGroups.ended.some(({ id }) => id === 'ended-pinned'), false);

  assert.match(PIN_UNAVAILABLE_WHEN_ENDED, /[Aa]rchive/,
    'the refusal both pin surfaces print must name the verb that does work');

  // ── Part 2: the navigator a person actually uses ─────────────────────────
  t.scenario('the fixture navigator builds, the server binds and the browser launches');
  await build({
    entryPoints: [fileURLToPath(new URL('./pin-navigator-fixture.tsx', import.meta.url))],
    outdir: scratch,
    bundle: true,
    platform: 'browser',
    format: 'esm',
    define: { 'import.meta.env.BASE_URL': '"/"' },
    entryNames: 'app',
    assetNames: 'asset-[hash]',
    external: ['/claude-icon.svg'],
    loader: { '.svg': 'dataurl', '.png': 'dataurl' },
    logLevel: 'silent'
  });
  await writeFile(join(scratch, 'index.html'), `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<link rel="stylesheet" href="/app.css"></head><body><div id="root"></div>
<script>
localStorage.setItem('sessions:servers', JSON.stringify([{id:'fixture',name:'Fixture Mac',host:'127.0.0.1',port:8787,isDefault:true}]));
localStorage.setItem('sessions:active-server','fixture');
localStorage.setItem('sessions:navigator-machine-scope','fixture');
</script>
<script type="module" src="/app.js"></script></body></html>`);

  server = createServer(async (request, response) => {
    const name = request.url === '/' ? 'index.html' : request.url.slice(1);
    try {
      const source = name === 'openai-icon.svg' || name === 'claude-icon.svg'
        ? join(publicDir, name)
        : join(scratch, name);
      const body = await readFile(source);
      const type = extname(name) === '.css'
        ? 'text/css'
        : extname(name) === '.js'
          ? 'text/javascript'
          : extname(name) === '.svg'
            ? 'image/svg+xml'
            : extname(name) === '.png'
              ? 'image/png'
              : 'text/html';
      response.writeHead(200, { 'content-type': type });
      response.end(body);
    } catch {
      response.writeHead(404);
      response.end();
    }
  });
  await t.bounded(
    new Promise((resolve) => server.listen(0, '127.0.0.1', resolve)),
    'the fixture static server to bind on 127.0.0.1',
    15_000
  );
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('fixture server did not bind');

  browser = await t.bounded(
    puppeteer.launch({ headless: true, args: ['--no-sandbox'] }),
    'puppeteer to launch a headless browser',
    60_000
  );
  const page = await browser.newPage();
  t.watch(page);
  page.setDefaultTimeout(15_000);
  page.setDefaultNavigationTimeout(15_000);
  await page.setViewport({ width: 1280, height: 1100, deviceScaleFactor: 1 });
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.goto(`http://127.0.0.1:${address.port}`, { waitUntil: 'domcontentloaded' });
  await t.waitForSelector(page, '#surface-navigator .session-nav-row', 'the navigator to render its first session row');

  // Which labelled section each row is rendered under. The group heads are the
  // user-visible answer to "where is this session?", so the assertions read
  // them rather than any internal id set.
  // Unpinned live rows now sit under project sections (one per named project,
  // plus "Other projects" for folders nobody has named) rather than a single
  // "Live" group. `liveRowsOf` gathers them so the assertions below keep
  // asking "is this session listed as a main session?" regardless of which
  // project section it landed in.
  const liveRowsOf = (groups) => Object.entries(groups)
    .filter(([label]) => label !== 'Pinned' && !label.startsWith('Ended') && !label.startsWith('Needs you'))
    .flatMap(([, ids]) => ids);
  const readGroups = () => page.evaluate(() => {
    const groups = {};
    for (const group of document.querySelectorAll('#surface-navigator .session-tree-group')) {
      const head = group.querySelector('.session-group-disclosure');
      const label = (head?.textContent ?? '').replace(/\s+/g, ' ').trim();
      groups[label] = Array.from(
        group.querySelectorAll('[data-session-id]'),
        (row) => row.getAttribute('data-session-id')
      );
    }
    return groups;
  });

  const openRowMenu = async (id) => {
    await page.$eval(
      `[data-session-actions="${id}"] .session-row-action-trigger`,
      (element) => element.click()
    );
    await t.waitForSelector(page, `[data-session-action-menu="${id}"]`, `the ••• menu for ${id} to open`);
    return page.evaluate((sessionId) => Array.from(
      document.querySelectorAll(`[data-session-action-menu="${sessionId}"] > button[role="menuitem"]`),
      (button) => ({
        label: (button.textContent ?? '').replace(/\s+/g, ' ').trim(),
        disabled: button.disabled,
        title: button.getAttribute('title') ?? ''
      })
    ), id);
  };

  t.scenario('the Pinned group exists and holds only the pinned main session');
  const initial = await readGroups();
  assert.ok('Pinned' in initial, `the navigator must render a "Pinned" section; saw ${Object.keys(initial).join(', ')}`);
  assert.deepEqual(initial.Pinned, ['pinned-manager'],
    'the Pinned group holds the pinned main session and does not leak its subagents');
  assert.ok(liveRowsOf(initial).includes('plain-manager'));
  assert.ok(!liveRowsOf(initial).includes('pinned-child'),
    'an agent-created helper must not leak into another main-session group');
  assert.ok(!liveRowsOf(initial).includes('pinned-manager'),
    'a pinned session must not also be listed under Live');
  assert.equal(await page.$('[data-session-id="pinned-child"]'), null,
    'the helper is available from the manager Subagents panel, not the left navigator');

  t.scenario('the row menu offers Pin, and using it moves the row into the Pinned group');
  // (a) The whole report: this item did not exist.
  const plainMenu = await openRowMenu('plain-manager');
  const plainPin = plainMenu.find((item) => item.label === 'Pin');
  assert.ok(plainPin, `the ••• menu must offer "Pin"; saw ${plainMenu.map((item) => item.label).join(' | ')}`);
  assert.equal(plainPin.disabled, false, 'a live session can be pinned');
  await page.$eval(
    '[data-session-action-menu="plain-manager"] .session-action-pin',
    (element) => element.click()
  );
  await t.waitForSelector(
    page,
    '#surface-navigator .session-tree-group.is-pinned [data-session-id="plain-manager"]',
    'the newly pinned session to appear in the Pinned group'
  );
  const afterPin = await readGroups();
  assert.ok(afterPin.Pinned.includes('plain-manager'));
  assert.ok(!liveRowsOf(afterPin).includes('plain-manager'),
    'pinning must remove the row from Live, not duplicate it');
  // Every live session in this fixture is now pinned, so the Live group is
  // empty. That must not read as "you have nothing going on": the navigator
  // throws Ended open when the working set is empty, and pinning is not a way
  // of emptying it.
  assert.equal(await page.$('[data-session-actions="ended-record"]'), null,
    'pinning the last unpinned session must not make the app act as though nothing is live');

  t.scenario('the same item now reads Unpin, and using it returns the row to Live');
  // (d) at the level a person sees it.
  const pinnedMenu = await openRowMenu('plain-manager');
  assert.ok(pinnedMenu.some((item) => item.label === 'Unpin'),
    `a pinned row's menu must read "Unpin"; saw ${pinnedMenu.map((item) => item.label).join(' | ')}`);
  assert.ok(!pinnedMenu.some((item) => item.label === 'Pin'),
    'the menu offers one pin verb, naming the state it moves to');
  await page.$eval(
    '[data-session-action-menu="plain-manager"] .session-action-pin',
    (element) => element.click()
  );
  await t.waitForFunction(
    page,
    () => !document.querySelector('#surface-navigator .session-tree-group.is-pinned [data-session-id="plain-manager"]'),
    'the unpinned session to leave the Pinned group'
  );
  const afterUnpin = await readGroups();
  assert.ok(liveRowsOf(afterUnpin).includes('plain-manager'),
    'unpinning returns the row to the group it came from');
  assert.deepEqual(afterUnpin.Pinned, ['pinned-manager'],
    'the Pinned group is back to exactly the main sessions that are still pinned');

  t.scenario('an ended session shows the pin refused, with the reason and the verb that works');
  // Ended is the one group that starts collapsed, and it opens itself when the
  // working set is empty — so toggle it only when the row is not already there,
  // rather than clicking it shut.
  if (!(await page.$('[data-session-actions="ended-record"]'))) {
    await page.$eval('#surface-navigator div.session-tree-group-head > button', (element) => element.click());
  }
  await t.waitForSelector(page, '[data-session-actions="ended-record"]', 'the ended session row to be listed');
  const endedMenu = await openRowMenu('ended-record');
  const endedPin = endedMenu.find((item) => item.label.startsWith('Pin'));
  assert.ok(endedPin, 'the pin item stays visible on an ended session rather than looking unimplemented');
  assert.equal(endedPin.disabled, true, 'the daemon answers 409 for an ended session, so the UI must not send it');
  assert.equal(endedPin.title, PIN_UNAVAILABLE_WHEN_ENDED,
    'the disabled item carries the daemon\'s own reason');
  assert.ok(endedMenu.some((item) => item.label.includes('Archive')),
    'the verb the refusal points at is in the same menu');

  assert.deepEqual(pageErrors, [], 'the navigator must not throw while pinning');

  t.pass('pin smoke: ok');
} finally {
  t.release();
  await closeBrowser(browser);
  await closeServer(server);
  await rm(scratch, { recursive: true, force: true });
}
