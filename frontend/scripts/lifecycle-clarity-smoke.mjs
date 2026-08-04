import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';

const source = async (path) => readFile(new URL(`../${path}`, import.meta.url), 'utf8');
const mux = await source('src/lib/wsMux.ts');
const [
  app,
  connection,
  navigator,
  details,
  history,
  view,
  tabs,
  popout,
  remote,
  grid,
  input,
  modelControl,
  machineMark,
  newSession,
  resumeDialog,
  continueElsewhere,
  productSidebar,
  settingsView,
  api,
  tabLabels,
  sessionsStore,
  styles
] = await Promise.all([
  source('src/App.tsx'),
  source('src/components/ConnectionStatus.tsx'),
  source('src/components/SessionNavigator.tsx'),
  source('src/components/SessionDetails.tsx'),
  source('src/components/SessionHistoryView.tsx'),
  source('src/components/SessionView.tsx'),
  source('src/components/SessionTabs.tsx'),
  source('src/components/SessionPopOutButton.tsx'),
  source('src/components/RemoteView.tsx'),
  source('src/components/GridView.tsx'),
  source('src/components/InputBar.tsx'),
  source('src/components/ComposerModelControl.tsx'),
  source('src/components/MachineMark.tsx'),
  source('src/components/NewSessionDialog.tsx'),
  source('src/components/ResumeDialog.tsx'),
  source('src/components/ContinueElsewhereButton.tsx'),
  source('src/components/ProductSidebar.tsx'),
  source('src/components/SettingsView.tsx'),
  source('src/api/sessionsd.ts'),
  source('src/lib/tabLabels.ts'),
  source('src/store/sessions.ts'),
  source('src/styles/globals.css')
]);

assert.doesNotMatch(app, /fromTerminalStatus/);
assert.match(app, /machine=\{machine\} hydrated=\{sessionsHydrated\} error=\{sessionsError\}/);
assert.doesNotMatch(connection, /terminalStatus|last known state/i);
assert.match(connection, /Can’t reach \$\{machine\}/);

assert.match(navigator, /RECENTLY_ENDED_DAYS = 7/);
assert.match(navigator, /RECENTLY_ENDED_LIMIT = 20/);
assert.match(navigator, /DisclosureChevron open=\{runningOpen\} \/> Live/);
assert.match(navigator, /DisclosureChevron open=\{endedOpen\} \/> Ended/);
assert.doesNotMatch(navigator, /> Later<|> Quiet<|Pin manager|Move to Later/);
assert.match(navigator, /session-tree-toggle/);
assert.match(navigator, /All ended sessions →/);
assert.doesNotMatch(navigator, /ENDED_CATEGORIES|Provider finished|Ended through Sessions/);
assert.doesNotMatch(navigator, /Problems/);
assert.doesNotMatch(navigator, /session-nav-summary/);
assert.match(navigator, />Resume <span aria-hidden>→<\/span><\/button>/);
assert.match(navigator, /draggable=\{movingId !== session\.id\}/);
assert.match(navigator, /text\/x-sessions-session-id/);
assert.match(navigator, /Start linked session…/);
assert.match(navigator, /Close tab <small>keeps running<\/small>/);
assert.match(navigator, /className="session-helper-summary"/);
assert.match(navigator, /session-nav-rollup/);
assert.match(navigator, /<summary>Fork <small>original stays here<\/small><\/summary>/);
assert.match(navigator, /'In Claude'/);
assert.match(navigator, /'In Codex'/);
assert.match(navigator, /ContinueElsewhereButton/);
assert.match(navigator, /createPortal/);
assert.match(navigator, /data-session-action-menu/);
assert.match(navigator, /className="session-row-action-trigger"/);
assert.match(navigator, /document\.addEventListener\('click', dismiss\)/);
assert.match(navigator, /event\.key !== 'Escape'/);
assert.doesNotMatch(navigator, /window\.confirm/);
assert.match(continueElsewhere, /createPortal/);
assert.match(continueElsewhere, /appearance === 'menuitem'/);
assert.match(resumeDialog, /preferredDestinationApplied/);
assert.doesNotMatch(resumeDialog, /preferredRemoteControl|resume-remote-control/);
assert.match(resumeDialog, /Remote Control follows the explicit choice for the destination machine in Settings/);
assert.match(navigator, /<MachineMark machine=\{machine\} size=\{17\} \/>/);
assert.doesNotMatch(navigator, /<span>\{machine\}<\/span>/);
assert.match(navigator, /<ProviderMark provider=\{providerName\} size=\{20\} \/>/);
assert.match(navigator, /className="session-continue-action" onClick=\{onContinue\}>Resume<\/button>/);
assert.match(app, /onContinue=\{\(\) => setDialogOpen\('resume'\)\}/);
assert.doesNotMatch(navigator, /session-mode-glyph/);
assert.match(tabLabels, /export function reconcileDurableTabLabels/);
assert.match(tabLabels, /const durable = session\.name\?\.trim\(\)/);
assert.match(sessionsStore, /reconcileDurableTabLabels\(fresh\)/);
assert.match(tabs, /setTabLabel\(id, name\)/);
assert.doesNotMatch(tabs, /setTabLabel\(id, name, session\?\.cwd\)/);

assert.match(details, />Session control</);
assert.match(details, /The conversation is kept and you can resume it later/);
assert.doesNotMatch(details, /Danger zone|Already ended|btn-danger/);
assert.doesNotMatch(history, /Delegate/);
assert.match(history, /read-only history/);
assert.doesNotMatch(history, /↻ Resume/);
assert.match(history, /Archive from list/);
assert.match(history, /continuationSession\(session, allSessions\)/);
assert.match(history, /Open \{continuationIsLive \? 'live ' : ''\}continuation/);
assert.match(history, /aria-label="Jump to latest message"/);
assert.match(history, /element\.scrollTo\(\{ top: element\.scrollHeight, behavior: 'smooth' \}\)/);
assert.doesNotMatch(history, /No resumable provider identity was found/);
assert.doesNotMatch(tabs, /onResume|tab-resume|tab-popout/);
assert.doesNotMatch(app, /onResume=\{\(\) => setDialogOpen\('resume'\)\}/);
assert.match(app, /const showManagerTabs = activeManagerTabs\.length > 1/);
assert.match(app, /sessionWorkspace && showManagerTabs/);
assert.match(app, /sessionId=\{sessionId\}[\s\S]*isActive[\s\S]*onStatusChange=\{setStatus\}/);
assert.match(popout, />Pop out<\/span>/);
assert.match(popout, /mode'\) === 'single'/);
assert.match(view, /No terminal for this Rich session/);
assert.match(view, /choose Continue conversation, then select Terminal/);
assert.match(view, /terminalAvailable=\{!richSession\}/);
assert.match(view, /Continuing the same Claude conversation in Terminal with Remote Control/);
assert.match(view, /Continuing the same Claude conversation in Terminal for slash commands/);
assert.doesNotMatch(view, /Not available in 0\.2\.7/);
assert.doesNotMatch(view, /↳ Delegate|resumed from seq/);
assert.match(view, /This Codex session uses its terminal interface/);
assert.match(view, />Okay<\/button>/);
assert.match(view, />Don’t show again<\/button>/);
assert.match(view, /session\.tool === 'codex'[\s\S]*!richSession/);
assert.doesNotMatch(view, /reading \{session\.tool === 'claude-code'/);
assert.match(view, /<MachineMark machine=\{serverDisplayName\(getActiveServer\(\), true\)\} size=\{18\} \/>/);
assert.doesNotMatch(view, /session-parser/);
assert.match(view, /sessions:terminal-notice-ack:/);
assert.match(view, /setTimeout\(\(\) => \{[\s\S]*setTerminalNoticeOpen\(true\);[\s\S]*\}, 400\)/);
assert.match(styles, /\.terminal-runtime-notice\s*\{[\s\S]*position:\s*absolute;/);
assert.match(styles, /@media \(max-width: 720px\)[\s\S]*\.terminal-runtime-notice\s*\{[\s\S]*position:\s*fixed;/);
assert.match(styles, /\.session-tree-toggle\s*\{[\s\S]*width:\s*30px;[\s\S]*height:\s*30px;/);
assert.match(styles, /\.session-ended-summary\s*\{[^}]*grid-template-columns:\s*minmax\(0,\s*1fr\);[^}]*min-width:\s*0;/);
assert.match(styles, /\.session-ended-summary\s*\{[^}]*position:\s*sticky;[^}]*top:\s*12px;/);
assert.match(styles, /\.session-ended-summary\s*>\s*\.session-ended-actions\s*\{[^}]*flex-wrap:\s*wrap;[^}]*max-width:\s*100%;/);
assert.match(styles, /\.session-history-jump-anchor\s*\{[^}]*position:\s*sticky;[^}]*bottom:\s*14px;/);
assert.match(styles, /\.product-command-button\s*\{[^}]*font-size:\s*13px;/);
assert.doesNotMatch(styles, /\.tab-resume|\.tab-popout/);
assert.match(styles, /\.session-row-action-menu \{ position: fixed/);
assert.doesNotMatch(styles, /\.session-ended-summary\.is-failed/);
assert.doesNotMatch(remote, /copyOnClickAtPointer|onClick=\{\(e\) => copy/);
assert.doesNotMatch(grid, /copyOnClickAtPointer|onClick=\{\(e\) => copy/);
assert.match(remote, /<CopyButton getText=\{m\.content\} iconOnly/);
assert.match(grid, /<CopyButton getText=\{m\.content\} iconOnly/);
assert.match(input, /<ComposerModelControl/);
assert.match(input, /Remote Control needs a Terminal session/);
assert.match(input, /This command was not sent as a chat message/);
assert.match(input, /Your draft is kept here and was not sent or queued/);
assert.match(input, /title: 'Message not sent'/);
assert.match(input, /Your draft is still here/);
assert.ok(input.includes("await send('\\x1b[200~' + text + '\\x1b[201~')"));
assert.doesNotMatch(mux, /return msg\.type === 'input' \|\|/);
assert.match(mux, /Sessions is reconnecting\. Your message was not sent\./);
assert.doesNotMatch(remote, />retry<\/button>/);
assert.match(remote, />restore draft<\/button>/);
assert.doesNotMatch(await source('src/hooks/useDispatch.ts'), /ENTER_RETRY_OFFSETS_MS|scheduleEnterRetries|send\('\\r'\)/);
assert.match(input, /\/rename\(\?:\\s\|\$\)/);
assert.match(remote, /if \(!terminalAvailable \|\| !latestFailedSend\)/);
assert.match(remote, /richSession=\{!terminalAvailable\}/);
assert.match(modelControl, /Next message/);
assert.match(modelControl, /listSessionModelOptions\(sessionId\)/);
assert.match(modelControl, /Applies to the next message\. Existing history is unchanged\./);
assert.match(machineMark, /aria-label=\{machine\}/);
assert.match(styles, /\.remote-message-actions\.is-agent\s*\{\s*justify-content:\s*flex-start;/);
assert.doesNotMatch(styles, /\.remote-bubble-assistant\s*\{[^}]*cursor:\s*copy;/);
assert.match(newSession, /\{browserOpen \? \([\s\S]*<DirectoryBrowser[\s\S]*\) : null\}/);
const launcherHero = newSession.indexOf("'Start a new session'");
const agentControl = newSession.indexOf('aria-label="Agent"');
const machineControl = newSession.indexOf('aria-label="Computer"');
const workspaceControl = newSession.indexOf('launcher-setup-control is-workspace');
const launcherComposer = newSession.indexOf('launcher-task-field launcher-composer');
const folderControl = newSession.indexOf('launcher-workspace-shell');
const advancedControl = newSession.indexOf('launcher-advanced');
const permissionsControl = newSession.indexOf('aria-label="Permissions"');
assert.ok(launcherHero > 0 && launcherHero < agentControl && agentControl < machineControl && machineControl < workspaceControl && workspaceControl < launcherComposer && launcherComposer < folderControl,
  'new-session must present agent, computer, and folder before the prompt');
assert.ok(permissionsControl > launcherComposer && permissionsControl < advancedControl,
  'permissions belong in the primary composer before Advanced');
assert.match(newSession, /Somewhere project/);
assert.doesNotMatch(newSession, /worktree|Developer isolation|Git copy/);
assert.doesNotMatch(newSession, /already has .* live sessions|Sessions will not stop or queue existing work/);
assert.match(newSession, /serverDisplayName\(selectedMachine, true\)/);
assert.match(newSession, /configuredMachines\.find\(\(machine\) => machine\.isDefault && isLocalServer\(machine\)\)/);
assert.match(newSession, /<ModelPicker[\s\S]*options=\{modelOptions\}/);
assert.match(newSession, /CLAUDE_MODEL_OPTIONS/);
assert.match(newSession, /launcher-composer-footer/);
assert.match(newSession, /listNewSessionCodexModels\(controller\.signal, machineId\)/);
assert.match(newSession, /create\(\{[\s\S]*\}, machineId\)/);
assert.match(newSession, /submitInitialRequest\(info\.id, task\.trim\(\), machineId\)/);
assert.match(newSession, /<DirectoryBrowser[\s\S]*serverId=\{machineId\}/);
assert.doesNotMatch(newSession, /resumeId|sessionsForCwd|--resume/);
assert.match(newSession, /event\.currentTarget\.form\?\.requestSubmit\(\)/);
assert.match(api, /createSession\(req: CreateSessionRequest, serverId\?: string\)/);
assert.match(api, /killSession\(id: string, reason = '', serverId\?: string\)/);
assert.match(api, /sendInput\(sessionId: string, data: string, serverId\?: string\)/);
assert.match(sessionsStore, /api\.killSession\(id, reason, serverId\)/);
assert.match(api, /\/api\/models\/codex/);
assert.match(api, /remoteControl/);
assert.doesNotMatch(newSession, /Provider default|More options|screen parsing|message parsing/);
assert.match(newSession, /<summary><strong>Advanced<\/strong>/);
assert.match(newSession, /if \(tool === 'claude-code'\) return 'terminal'/);
assert.match(newSession, /Conversation \+ Terminal/);
assert.match(newSession, /Uses this machine’s consent choice from Settings/);
assert.doesNotMatch(newSession, /runtimeMode === 'rich'\s*\?\s*\{\s*\.\.\.claudeOptions,\s*remoteControl: 'off'/);
assert.doesNotMatch(newSession, /Choose Terminal for Claude slash commands and Remote Control/);
assert.match(settingsView, /Enabled for new Claude sessions/);
assert.match(settingsView, /Keep sessions local/);
assert.match(productSidebar, />Send feedback<\/span>/);
assert.match(productSidebar, /onNavigate\('feedback'\)/);
assert.match(settingsView, /Help shape Sessions/);
assert.match(settingsView, /Share an idea or win/);
assert.match(settingsView, /Report a bug/);
assert.doesNotMatch(settingsView, /Reporting from an agent/);
assert.match(resumeDialog, /Continue an earlier chat/);
assert.match(resumeDialog, /\(\['all', 'claude', 'codex'\] as const\)/);
assert.match(resumeDialog, /s\.title\?\.toLowerCase\(\)\.includes\(q\)/);
assert.match(resumeDialog, /const title = session\.title\?\.trim\(\) \|\| msg/);
assert.match(resumeDialog, /Search titles, requests, or workspaces/);

const browser = await puppeteer.launch({ headless: true });
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 900, height: 600 });
  await page.setContent(`
    <style>${styles}</style>
    <main class="session-view view-remote" style="width: 900px; height: 600px">
      <header class="session-active-header">
        <div class="session-active-copy">
          <span class="session-parent-breadcrumb">Manager session</span>
          <div class="session-active-title-row">
            <h1>Terminal compatibility check</h1>
            <span class="session-live-pill">Idle</span>
            <span class="session-runtime-anchor">
              <span class="session-runtime-badge is-terminal">Terminal</span>
              <aside id="terminal-notice" class="terminal-runtime-notice">
                <strong>This is a Terminal session</strong>
                <p>The conversation may be delayed or incomplete.</p>
                <div class="terminal-runtime-notice-actions"><button class="btn">Okay</button></div>
              </aside>
            </span>
          </div>
        </div>
      </header>
      <div class="session-toolbar"><div class="view-toggle">Conversation</div></div>
      <div id="session-body" class="session-body"></div>
    </main>
  `);
  const withNotice = await page.evaluate(() => ({
    bodyHeight: document.querySelector('#session-body').getBoundingClientRect().height,
    noticeWidth: document.querySelector('#terminal-notice').getBoundingClientRect().width,
    noticeHeight: document.querySelector('#terminal-notice').getBoundingClientRect().height
  }));
  await page.$eval('#terminal-notice', (notice) => notice.remove());
  const withoutNoticeHeight = await page.$eval('#session-body', (body) => body.getBoundingClientRect().height);
  assert.equal(withNotice.bodyHeight, withoutNoticeHeight, 'terminal notice must not resize the session body');
  assert.ok(withNotice.noticeWidth <= 322, `terminal notice should remain a bubble, got ${withNotice.noticeWidth}px`);
  assert.ok(withNotice.noticeHeight < 240, `terminal notice should remain compact, got ${withNotice.noticeHeight}px`);

  await page.setViewport({ width: 460, height: 500 });
  await page.setContent(`
    <style>${styles}</style>
    <section id="ended-card" class="session-ended-summary" style="width: 392px; box-sizing: border-box">
      <div><strong>Conversation ended</strong><span>Today at 10:14 AM</span></div>
      <p>The provider finished normally and the exact conversation is still available.</p>
      <p class="session-ended-read-only">Viewing does not resume or send anything.</p>
      <div class="session-ended-actions">
        <button class="btn btn-primary">Continue conversation →</button>
        <button class="btn btn-secondary">Archive from list</button>
      </div>
    </section>
  `);
  const endedActionBounds = await page.evaluate(() => {
    const card = document.querySelector('#ended-card').getBoundingClientRect();
    return [...document.querySelectorAll('.session-ended-actions .btn')].map((button) => {
      const bounds = button.getBoundingClientRect();
      return { left: bounds.left, right: bounds.right, cardLeft: card.left, cardRight: card.right };
    });
  });
  for (const bounds of endedActionBounds) {
    assert.ok(bounds.left >= bounds.cardLeft, 'ended-session action must stay inside the card on the left');
    assert.ok(bounds.right <= bounds.cardRight, 'ended-session action must stay inside the card on the right');
  }

  await page.setViewport({ width: 900, height: 620 });
  await page.setContent(`
    <style>${styles}</style>
    <main id="history-body" class="session-history-body" style="width: 860px; height: 500px">
      <section class="session-history-transcript">
        <div id="sticky-ended-card" class="session-ended-summary">
          <div><strong>Ended</strong><span>Today at 10:14 AM</span></div>
          <p>The runtime ended. The saved conversation remains available.</p>
        </div>
        ${Array.from({ length: 32 }, (_, index) => `<article class="session-history-message"><p>Conversation message ${index + 1}</p></article>`).join('')}
      </section>
      <div class="session-history-jump-anchor">
        <button id="history-jump" class="scroll-to-bottom">↓</button>
      </div>
    </main>
  `);
  await page.$eval('#history-body', (element) => { element.scrollTop = 640; });
  const jumpBounds = await page.evaluate(() => {
    const body = document.querySelector('#history-body').getBoundingClientRect();
    const card = document.querySelector('#sticky-ended-card').getBoundingClientRect();
    const jump = document.querySelector('#history-jump').getBoundingClientRect();
    return {
      bodyTop: body.top,
      bodyBottom: body.bottom,
      bodyRight: body.right,
      cardTop: card.top,
      cardBottom: card.bottom,
      jumpBottom: jump.bottom,
      jumpRight: jump.right
    };
  });
  assert.ok(jumpBounds.cardTop >= jumpBounds.bodyTop, 'ended-session card must remain inside the history viewport');
  assert.ok(jumpBounds.cardTop <= jumpBounds.bodyTop + 16, 'ended-session card must stick near the top while history scrolls');
  assert.ok(jumpBounds.cardBottom <= jumpBounds.bodyBottom, 'ended-session card must not obscure the entire history viewport');
  assert.ok(jumpBounds.jumpBottom <= jumpBounds.bodyBottom, 'jump-to-latest button must stay inside the history viewport');
  assert.ok(jumpBounds.jumpRight <= jumpBounds.bodyRight, 'jump-to-latest button must stay inside the history viewport');
} finally {
  await browser.close();
}

const scratch = await mkdtemp(join(tmpdir(), 'sessions-mode-smoke-'));
const output = join(scratch, 'session-mode.mjs');
try {
  await build({
    entryPoints: ['src/lib/sessionMode.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });
  const { sessionMode, sessionModeGlyph, sessionModeName } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const rich = { kind: 'codex-app-server', tool: 'codex' };
  const terminal = { kind: 'pty', tool: 'claude-code', args: [] };
  const remote = { kind: 'pty', tool: 'claude-code', args: ['--remote-control'] };
  assert.equal(sessionMode(rich), 'rich');
  assert.equal(sessionModeGlyph(rich), '◆');
  assert.equal(sessionModeName(rich), 'Rich — Codex app-server');
  assert.equal(sessionMode(terminal), 'terminal');
  assert.equal(sessionModeGlyph(terminal), '▮');
  assert.equal(sessionModeName(terminal), 'Claude interactive — Conversation + Terminal');
  assert.equal(sessionModeName(remote), 'Claude Remote Control — Conversation + Terminal');
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('lifecycle clarity smoke: ok');
