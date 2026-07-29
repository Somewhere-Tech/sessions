#!/usr/bin/env node

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

function usage(message) {
  if (message) console.error(`error: ${message}`);
  console.error(`usage: node scripts/render-updater-manifest.mjs \\
  --version 1.2.3 \\
  --artifact /path/to/Sessions.app.tar.gz \\
  --url https://github.com/somewhere-tech/sessions/releases/download/v1.2.3/Sessions.app.tar.gz \\
  --target darwin-aarch64 \\
  --output /path/to/latest.json \\
  [--base /path/to/existing.json] \\
  [--notes "What changed"] [--notes-file /path/to/notes.md] [--pub-date ISO-8601]`);
  process.exit(2);
}

const args = new Map();
for (let index = 2; index < process.argv.length; index += 2) {
  const key = process.argv[index];
  const value = process.argv[index + 1];
  if (!key?.startsWith('--') || value === undefined) usage(`invalid argument ${key ?? ''}`);
  args.set(key.slice(2), value);
}

const required = ['version', 'artifact', 'url', 'target', 'output'];
for (const key of required) {
  if (!args.get(key)) usage(`--${key} is required`);
}

const version = args.get('version');
if (!/^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/.test(version)) {
  usage('--version must be a semantic version without a leading v');
}

const artifact = path.resolve(args.get('artifact'));
const signaturePath = `${artifact}.sig`;
if (!fs.statSync(artifact, { throwIfNoEntry: false })?.isFile()) {
  usage(`artifact does not exist: ${artifact}`);
}
if (!fs.statSync(signaturePath, { throwIfNoEntry: false })?.isFile()) {
  usage(`signature does not exist: ${signaturePath}`);
}

let artifactUrl;
try {
  artifactUrl = new URL(args.get('url'));
} catch {
  usage('--url must be an absolute URL');
}
if (artifactUrl.protocol !== 'https:') usage('--url must use HTTPS');
if (!artifactUrl.pathname.includes(`/v${version}/`)) {
  usage(`--url must contain the immutable release segment /v${version}/`);
}
if (decodeURIComponent(path.posix.basename(artifactUrl.pathname)) !== path.basename(artifact)) {
  usage('--url basename must match the artifact basename');
}

const signature = fs.readFileSync(signaturePath, 'utf8').trim();
if (!signature) usage(`signature is empty: ${signaturePath}`);

let notes = args.get('notes') ?? '';
if (args.has('notes-file')) {
  if (args.has('notes')) usage('use either --notes or --notes-file, not both');
  notes = fs.readFileSync(path.resolve(args.get('notes-file')), 'utf8').trim();
}

let platforms = {};
let basePubDate = '';
if (args.has('base')) {
  const basePath = path.resolve(args.get('base'));
  if (!fs.statSync(basePath, { throwIfNoEntry: false })?.isFile()) {
    usage(`base manifest does not exist: ${basePath}`);
  }
  let base;
  try {
    base = JSON.parse(fs.readFileSync(basePath, 'utf8'));
  } catch {
    usage(`base manifest is not valid JSON: ${basePath}`);
  }
  if (base?.version !== version) {
    usage(`base manifest version must be ${version}`);
  }
  if (!base.platforms || typeof base.platforms !== 'object' || Array.isArray(base.platforms)) {
    usage('base manifest must contain a platforms object');
  }
  for (const [target, entry] of Object.entries(base.platforms)) {
    if (!target || !entry || typeof entry !== 'object' ||
        typeof entry.url !== 'string' || !entry.url ||
        typeof entry.signature !== 'string' || !entry.signature) {
      usage(`base manifest platform ${target || '<empty>'} is incomplete`);
    }
  }
  if (Object.hasOwn(base.platforms, args.get('target'))) {
    usage(`base manifest already contains target ${args.get('target')}`);
  }
  platforms = { ...base.platforms };
  if (!args.has('notes-file') && !args.has('notes')) notes = typeof base.notes === 'string' ? base.notes : '';
  basePubDate = typeof base.pub_date === 'string' ? base.pub_date : '';
}

const pubDate = args.get('pub-date') ?? (basePubDate || new Date().toISOString());
if (Number.isNaN(Date.parse(pubDate))) usage('--pub-date must be a valid ISO-8601 timestamp');

platforms[args.get('target')] = {
  signature,
  url: artifactUrl.href
};

const manifest = {
  version,
  notes,
  pub_date: new Date(pubDate).toISOString(),
  platforms
};

const output = path.resolve(args.get('output'));
fs.mkdirSync(path.dirname(output), { recursive: true });
fs.writeFileSync(output, `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o644 });
console.log(output);
