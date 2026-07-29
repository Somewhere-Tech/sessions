#!/usr/bin/env node
// Verifies the contentRender pipeline (ANSI→HTML, then marked, then
// linkify) produces the right HTML for a representative Claude
// response. Doesn't need a running sessionsd / live Claude — just feeds
// strings through the same function the browser uses.

const path = require('node:path');
const fs = require('node:fs');
const os = require('node:os');

const FRONTEND_ROOT = path.resolve(__dirname, '..');
const esbuild = require(path.join(FRONTEND_ROOT, 'node_modules', 'esbuild'));

const tmp = path.join(os.tmpdir(), `sessions-md-smoke-${process.pid}.cjs`);
esbuild.buildSync({
  entryPoints: [path.join(FRONTEND_ROOT, 'src', 'lib', 'contentRender.ts')],
  bundle: true,
  platform: 'node',
  format: 'cjs',
  target: 'node20',
  outfile: tmp,
  logLevel: 'silent'
});
let mod;
try { mod = require(tmp); } finally { try { fs.unlinkSync(tmp); } catch {} }
const { renderContent } = mod;

const fixtures = [
  {
    name: 'plain bold + italic + inline code',
    md: 'Hello **bold**, *italic*, and `inline code`.',
    expectIncludes: ['<strong>bold</strong>', '<em>italic</em>', '<code>inline code</code>']
  },
  {
    name: 'fenced code block gets a copy button',
    md: 'Here is a script:\n\n```bash\necho hello\nexit 0\n```\n',
    expectIncludes: ['<div class="code-pre-wrap">', 'data-code-copy', '<pre>', 'echo hello', '</pre>']
  },
  {
    name: 'bullet list',
    md: '- one\n- two\n- three',
    expectIncludes: ['<ul>', '<li>one</li>', '<li>two</li>', '<li>three</li>', '</ul>']
  },
  {
    name: 'heading',
    md: '## Section header\n\nbody text',
    expectIncludes: ['<h2', 'Section header</h2>']
  },
  {
    name: 'autolink-style link',
    md: '[Anthropic](https://anthropic.com)',
    expectIncludes: ['<a href="https://anthropic.com">Anthropic</a>']
  },
  {
    name: 'file path → vscode://file/ link (relative, with cwd)',
    md: 'See `src/foo.ts:42` for details.',
    cwd: '/Users/test/proj',
    // path is inside backticks so it becomes <code>, and linkify only
    // walks text outside <a> — should still pick up paths inside <code>.
    expectIncludes: ['vscode://file/Users/test/proj/src/foo.ts:42']
  },
  {
    name: 'ANSI colors survive into HTML spans',
    md: '\x1b[38;2;110;255;140mgreen text\x1b[0m next',
    // anser inline-styles for truecolor.
    expectIncludesRegex: [/style="color\s*:\s*rgb\(110\s*,\s*255\s*,\s*140\)/]
  },
  // --- Link-scheme XSS coverage (sanitizeAnchorHrefs) ---
  {
    name: 'javascript: link is neutralized to href="#"',
    md: '[click me](javascript:alert(document.cookie))',
    expectIncludes: ['href="#"', '>click me</a>'],
    expectExcludes: ['javascript:']
  },
  {
    name: 'mixed-case JavaScript: is neutralized (schemes are case-insensitive)',
    md: '[x](JavaScript:alert(1))',
    expectIncludes: ['href="#"'],
    expectExcludes: ['JavaScript:alert', 'javascript:alert']
  },
  {
    name: 'data: link is neutralized',
    md: '[d](data:text/html,<script>alert(1)</script>)',
    expectIncludes: ['href="#"'],
    expectExcludes: ['href="data:']
  },
  {
    name: 'vbscript: link is neutralized',
    md: '[v](vbscript:msgbox(1))',
    expectIncludes: ['href="#"'],
    expectExcludes: ['vbscript:']
  },
  {
    name: 'safe schemes pass through unchanged',
    md: '[h](https://anthropic.com) [m](mailto:a@b.com) [rel](./docs/x.md) [hash](#sec)',
    expectIncludes: [
      'href="https://anthropic.com"',
      'href="mailto:a@b.com"',
      'href="./docs/x.md"',
      'href="#sec"'
    ]
  },
  {
    name: 'nothing weird with empty input',
    md: '',
    expectExact: ''
  }
];

let pass = 0, fail = 0;
for (const fx of fixtures) {
  let html;
  try {
    html = renderContent(fx.md, fx.cwd);
  } catch (err) {
    console.error(`FAIL [${fx.name}]: threw ${err.message}`);
    fail++;
    continue;
  }
  if (fx.expectExact !== undefined) {
    if (html.trim() !== fx.expectExact.trim()) {
      console.error(`FAIL [${fx.name}]: expected exact ${JSON.stringify(fx.expectExact)}, got ${JSON.stringify(html)}`);
      fail++;
      continue;
    }
  }
  let ok = true;
  for (const needle of fx.expectIncludes ?? []) {
    if (!html.includes(needle)) {
      console.error(`FAIL [${fx.name}]: missing '${needle}' in output:\n${html}`);
      ok = false;
      break;
    }
  }
  if (ok) {
    for (const re of fx.expectIncludesRegex ?? []) {
      if (!re.test(html)) {
        console.error(`FAIL [${fx.name}]: regex ${re} did not match:\n${html}`);
        ok = false;
        break;
      }
    }
  }
  if (ok) {
    for (const banned of fx.expectExcludes ?? []) {
      if (html.includes(banned)) {
        console.error(`FAIL [${fx.name}]: should NOT contain '${banned}' in output:\n${html}`);
        ok = false;
        break;
      }
    }
  }
  if (!ok) { fail++; continue; }
  console.log(`PASS [${fx.name}]`);
  pass++;
}

console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail === 0 ? 0 : 1);
