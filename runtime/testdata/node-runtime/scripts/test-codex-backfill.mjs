// Integration regression for Codex rollout attach backfill and byte-offset
// handoff. A real rollout is read from ~/.codex/sessions, copied to an
// isolated temp directory, and only the copy is appended.
//   npx tsx --test scripts/test-codex-backfill.mjs

import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import * as fsp from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { normalizeCodexRolloutLine } from '../src/codexNormalize.js';
import { watchCodexRollout } from '../src/codexWatcher.js';

const sessionsDir = path.join(os.homedir(), '.codex', 'sessions');
const readByteLimit = 16 * 1024 * 1024;

function rolloutCandidates() {
  const files = [];
  const stack = [sessionsDir];
  while (stack.length > 0) {
    const dir = stack.pop();
    if (!dir) continue;
    let entries;
    try {
      entries = fs.readdirSync(dir, { withFileTypes: true });
    } catch {
      continue;
    }
    for (const entry of entries) {
      const entryPath = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        stack.push(entryPath);
      } else if (entry.isFile() && entry.name.startsWith('rollout-') && entry.name.endsWith('.jsonl')) {
        const stat = fs.statSync(entryPath);
        files.push({ path: entryPath, size: stat.size, mtimeMs: stat.mtimeMs });
      }
    }
  }
  return files.sort((a, b) => {
    const size = a.size - b.size;
    return size !== 0 ? size : b.mtimeMs - a.mtimeMs;
  });
}

function tailHasNormalizedEvent(candidate) {
  const start = Math.max(0, candidate.size - readByteLimit);
  const length = candidate.size - start;
  const fd = fs.openSync(candidate.path, 'r');
  const buf = Buffer.allocUnsafe(length);
  let bytesRead;
  try {
    bytesRead = fs.readSync(fd, buf, 0, length, start);
  } finally {
    fs.closeSync(fd);
  }
  let text = buf.subarray(0, bytesRead).toString('utf8');
  if (start > 0) {
    const firstNewline = text.indexOf('\n');
    text = firstNewline === -1 ? '' : text.slice(firstNewline + 1);
  }
  const lines = text.split('\n').slice(-2_000);
  return lines.some((line, lineIndex) => {
    if (!line.trim()) return false;
    try {
      const normalized = normalizeCodexRolloutLine(JSON.parse(line), {
        rolloutBasename: path.basename(candidate.path),
        lineIndex
      });
      return normalized.events.length > 0;
    } catch {
      return false;
    }
  });
}

function findRealRollout() {
  const candidate = rolloutCandidates().find(tailHasNormalizedEvent);
  if (!candidate) {
    throw new Error(`no real rollout with normalized events found under ${sessionsDir}`);
  }
  return candidate;
}

function eventText(event) {
  const content = event?.message?.content;
  if (!Array.isArray(content)) return '';
  return content
    .map((block) => block && typeof block === 'object' && typeof block.text === 'string' ? block.text : '')
    .join('\n');
}

async function waitFor(predicate, message, timeoutMs = 5_000) {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error(message);
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
}

test('backfills a real rollout and hands off to appended bytes exactly once', async (t) => {
  const source = findRealRollout();
  const tempDir = await fsp.mkdtemp(path.join(os.tmpdir(), 'pretty-codex-backfill-'));
  const fixturePath = path.join(tempDir, path.basename(source.path));
  await fsp.copyFile(source.path, fixturePath);

  const fixture = await fsp.open(fixturePath, 'r+');
  try {
    const stat = await fixture.stat();
    if (stat.size > 0) {
      const lastByte = Buffer.allocUnsafe(1);
      await fixture.read(lastByte, 0, 1, stat.size - 1);
      if (lastByte[0] !== 0x0a) await fixture.appendFile('\n');
    }
  } finally {
    await fixture.close();
  }

  const events = [];
  const workingStates = [];
  const marker = `pretty-backfill-handoff-${crypto.randomUUID()}`;
  const watcher = await watchCodexRollout({
    cwd: '',
    args: [],
    createdAt: Date.now(),
    rolloutPath: fixturePath,
    initialDelayMs: 0,
    pollIntervalMs: 25
  });
  watcher.emitter.on('event', (event) => events.push(event));
  watcher.emitter.on('working', (working) => workingStates.push(working));

  try {
    await waitFor(
      () => watcher.path() === fixturePath && events.length > 0,
      'watcher did not emit immediate backfill events'
    );
    const backfillCount = events.length;
    assert.ok(backfillCount > 0);
    assert.ok(backfillCount <= 2_000, `backfill emitted ${backfillCount} events`);
    assert.equal(events.some((event) => eventText(event).includes(marker)), false);

    const workingBeforeAppend = workingStates.length;
    const appendedEvent = JSON.stringify({
      timestamp: new Date().toISOString(),
      type: 'response_item',
      payload: {
        type: 'message',
        role: 'assistant',
        content: [{ type: 'output_text', text: marker }]
      }
    });
    const offsetSentinel = JSON.stringify({
      timestamp: new Date().toISOString(),
      type: 'event_msg',
      payload: { type: 'task_started' }
    });
    await fsp.appendFile(fixturePath, `${appendedEvent}\n${offsetSentinel}\n`);

    await waitFor(
      () => events.filter((event) => eventText(event).includes(marker)).length >= 1
        && workingStates.length > workingBeforeAppend,
      'watcher did not emit the appended live event'
    );
    await new Promise((resolve) => setTimeout(resolve, 150));

    assert.equal(
      events.filter((event) => eventText(event).includes(marker)).length,
      1,
      'appended normalized event must be emitted exactly once'
    );
    assert.equal(
      workingStates.length - workingBeforeAppend,
      1,
      'non-deduplicated offset sentinel must be consumed exactly once'
    );
    t.diagnostic(`source (read-only): ${path.relative(sessionsDir, source.path)}`);
    t.diagnostic(`immediate normalized backfill events: ${backfillCount}`);
    t.diagnostic('appended normalized event emissions: 1');
  } finally {
    watcher.close();
    await fsp.rm(tempDir, { recursive: true, force: true });
  }
});
