// Per-file setup for the capability suite.
//
// Two jobs:
//
//   1. HARD NETWORK BAN. No test in this suite is allowed to reach a real
//      daemon. `fetch`, `WebSocket`, `XMLHttpRequest` and `EventSource` are
//      replaced with throwers before any test file body runs. A test that
//      forgets to install the fake daemon fails with a sentence naming the URL
//      it tried to dial, instead of silently succeeding against whatever
//      sessionsd happens to be listening on localhost:8787.
//
//   2. jsdom gaps that are environment, not product. Only APIs jsdom has
//      genuinely not implemented are filled in — never a product module.
//      Nothing here changes what a component computes; if a component needs a
//      stub to behave correctly, that is a finding and it belongs in the
//      report, not in this file.
import { afterEach, beforeEach } from 'vitest';
import { cleanup } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';

const OFFLINE = (what: string) => (target: unknown): never => {
  const where = typeof target === 'string'
    ? target
    : target && typeof target === 'object' && 'url' in target
      ? String((target as { url: unknown }).url)
      : String(target);
  throw new Error(
    `capability suite: ${what} to ${where} escaped the fake daemon.\n`
    + 'These tests never contact a live sessionsd. Install the in-process fake '
    + 'with installFakeDaemon() in the test body, and add the missing route to '
    + 'tests/capability/fake-daemon.ts.'
  );
};

class BannedSocket {
  constructor(url: string) {
    OFFLINE('a WebSocket')(url);
  }
}

function banNetwork(): void {
  globalThis.fetch = OFFLINE('a fetch') as unknown as typeof fetch;
  globalThis.WebSocket = BannedSocket as unknown as typeof WebSocket;
  // jsdom's XHR and EventSource would otherwise open real sockets.
  globalThis.XMLHttpRequest = class {
    open(_method: string, url: string): void { OFFLINE('an XMLHttpRequest')(url); }
  } as unknown as typeof XMLHttpRequest;
  globalThis.EventSource = class {
    constructor(url: string) { OFFLINE('an EventSource')(url); }
  } as unknown as typeof EventSource;
}

banNetwork();

// ── jsdom gaps ─────────────────────────────────────────────────────────────
if (!globalThis.matchMedia) {
  globalThis.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false
  })) as unknown as typeof matchMedia;
}

if (!globalThis.ResizeObserver) {
  globalThis.ResizeObserver = class {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  } as unknown as typeof ResizeObserver;
}

if (!globalThis.IntersectionObserver) {
  globalThis.IntersectionObserver = class {
    readonly root = null;
    readonly rootMargin = '';
    readonly thresholds: number[] = [];
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
    takeRecords(): [] { return []; }
  } as unknown as typeof IntersectionObserver;
}

// jsdom implements neither of these on HTMLElement.
Element.prototype.scrollIntoView ??= function scrollIntoView(): void {};
Element.prototype.scrollTo ??= function scrollTo(): void {};
if (!globalThis.crypto?.randomUUID) {
  Object.defineProperty(globalThis.crypto ?? (globalThis.crypto = {} as Crypto), 'randomUUID', {
    value: () => 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (c) => {
      const r = (Math.random() * 16) | 0;
      return (c === 'x' ? r : (r & 0x3) | 0x8).toString(16);
    }),
    configurable: true
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  banNetwork();
});
