// CAPABILITY: the app only uses web APIs the phones it is built for actually
// have.
//
// The declared baseline is what the mobile projects say: iOS 14.0
// (src-tauri/gen/apple/project.yml, the Podfile and IPHONEOS_DEPLOYMENT_TARGET)
// and Android minSdk 24. jsdom has every API in this list, and so does every
// laptop the tests run on, so nothing else here can catch a call that would
// throw on a phone — only reading the source can.
//
// Versions are from MDN's browser-compat-data (github.com/mdn/browser-compat-data,
// main, read September 2026); `safari_ios: mirror` there means the iOS version
// equals the Safari version quoted.
import { readFileSync, readdirSync } from 'node:fs';
import { join, relative } from 'node:path';
import { describe, expect, it } from 'vitest';

interface BannedAPI {
  /** The call as it appears in source. */
  call: string;
  /** Why it cannot ship, in the terms of the baseline. */
  why: string;
  /** Files that may contain it because they are the guarded way to use it. */
  allow?: string[];
}

const BANNED: BannedAPI[] = [
  { call: 'AbortSignal.any(', why: 'iOS 17.4' },
  { call: 'AbortSignal.timeout(', why: 'iOS 16' },
  { call: '.at(', why: 'Array/String.prototype.at is iOS 15.4' },
  { call: '.findLast(', why: 'iOS 15.4' },
  { call: '.findLastIndex(', why: 'iOS 15.4' },
  { call: '.toSorted(', why: 'iOS 16' },
  { call: '.toReversed(', why: 'iOS 16' },
  { call: '.toSpliced(', why: 'iOS 16' },
  { call: 'Object.hasOwn(', why: 'iOS 15.4' },
  { call: 'Object.groupBy(', why: 'iOS 17.4' },
  { call: 'Map.groupBy(', why: 'iOS 17.4' },
  { call: 'Array.fromAsync(', why: 'iOS 16.4' },
  { call: 'Promise.withResolvers(', why: 'iOS 17.4' },
  { call: 'URL.canParse(', why: 'iOS 17' },
  { call: 'checkVisibility(', why: 'iOS 17.4' },
  { call: 'showModal(', why: 'the dialog element is iOS 15.4' },
  { call: 'requestIdleCallback(', why: 'not shipped in Safari at all' },
  { call: 'showOpenFilePicker(', why: 'not shipped in Safari at all' },
  { call: 'structuredClone(', why: 'iOS 15.4 (MDN docs; not read from the compat data here)' },
  // Available since iOS 13.1, but only in a secure context. Sessions is
  // reached over plain HTTP on a LAN address more often than not, where
  // navigator.clipboard is undefined and the call throws.
  { call: 'navigator.clipboard', why: 'undefined on the plain-HTTP origins phones use', allow: ['lib/copyText.ts'] },
  // Same shape: iOS 15.4, and secure-context only before that.
  { call: 'crypto.randomUUID(', why: 'iOS 15.4, and secure contexts only', allow: ['lib/uuid.ts'] }
];

function sourceFiles(directory: string, found: string[] = []): string[] {
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      sourceFiles(path, found);
      continue;
    }
    if (/\.(ts|tsx)$/.test(entry.name)) found.push(path);
  }
  return found;
}

function allowedSourcePath(path: string, allowed: string[] | undefined): boolean {
  return allowed?.includes(path.replace(/\\/g, '/')) ?? false;
}

describe('capability: the shipped code stays inside the declared WebView baseline', () => {
  it('calls nothing the baseline phones lack', () => {
    const root = join(process.cwd(), 'src');
    const offenders: string[] = [];
    for (const path of sourceFiles(root)) {
      const sourcePath = relative(root, path);
      const source = readFileSync(path, 'utf8');
      for (const banned of BANNED) {
        if (!source.includes(banned.call)) continue;
        if (allowedSourcePath(sourcePath, banned.allow)) continue;
        offenders.push(`${sourcePath}: ${banned.call} — ${banned.why}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it('matches only exact guarded helper paths on POSIX and Windows', () => {
    for (const separator of ['/', '\\']) {
      for (const file of ['copyText.ts', 'uuid.ts']) {
        expect(allowedSourcePath(`lib${separator}${file}`, [`lib/${file}`])).toBe(true);
        expect(allowedSourcePath(`other${separator}${file}`, [`lib/${file}`])).toBe(false);
        expect(allowedSourcePath(`lib${separator}${file}.extra`, [`lib/${file}`])).toBe(false);
        expect(allowedSourcePath(`lib${separator}${file}`, undefined)).toBe(false);
      }
    }
  });

  // The two guarded ways to reach an API the baseline gates rather than lacks.
  // Their existence is the reason the entries above carry an exception at all.
  it('keeps the guarded helpers that own those two APIs', () => {
    const clipboard = readFileSync(join(process.cwd(), 'src/lib/copyText.ts'), 'utf8');
    expect(clipboard).toContain('isSecureContext');
    expect(clipboard).toContain('execCommand');
    const uuid = readFileSync(join(process.cwd(), 'src/lib/uuid.ts'), 'utf8');
    expect(uuid).toContain("typeof c.randomUUID === 'function'");
    expect(uuid).toContain('getRandomValues');
  });
});
