// An ambiguous legacy submit must preserve its uncertainty and never replay.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { attachSession, submitSessionMessage, type SessionChannel } from '../../src/lib/wsMux';
import type { MuxClientMsg } from '../../src/types';

class FixtureSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  static instances: FixtureSocket[] = [];
  readyState = FixtureSocket.CONNECTING;
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  sent: MuxClientMsg[] = [];
  constructor() { FixtureSocket.instances.push(this); }
  open(): void { this.readyState = FixtureSocket.OPEN; this.onopen?.(); }
  close(): void { this.readyState = FixtureSocket.CLOSED; this.onclose?.(); }
  send(data: string): void { this.sent.push(JSON.parse(data) as MuxClientMsg); }
  reply(data: unknown): void { this.onmessage?.({ data: JSON.stringify(data) }); }
}

let sequence = 0;
let channel: SessionChannel;
let url: string;
let socket: FixtureSocket;
beforeEach(() => {
  vi.useFakeTimers();
  FixtureSocket.instances = [];
  globalThis.WebSocket = FixtureSocket as unknown as typeof WebSocket;
  url = `ws://fixture.invalid/mux-${sequence++}`;
  channel = attachSession(url, 'legacy', {
    onMessage: () => {}, onStatus: () => {},
    getResume: () => ({ lastSeq: 0, claudeEventsSince: 0, outputReplay: false, claudeReplay: true, claudeLive: true })
  });
  socket = FixtureSocket.instances[0]!;
});

afterEach(async () => {
  channel.detach();
  await vi.advanceTimersByTimeAsync(15_000);
  vi.useRealTimers();
});

describe('capability: legacy submit delivery evidence', () => {
  it('reports a connection refusal as unsent before any input leaves', async () => {
    await expect(submitSessionMessage(url, 'legacy', 'whole message')).rejects.toMatchObject({ deliveryStatus: 'not-delivered' });
    expect(socket.sent).toEqual([]);
  });

  it('keeps a suffix-only provider result unknown and carries the server reason', async () => {
    socket.open();
    const result = submitSessionMessage(url, 'legacy', 'whole message');
    const request = socket.sent.find(msg => msg.type === 'submit')!;
    socket.reply({ type: 'submitAck', requestId: request.requestId, sessionId: 'legacy', ok: false, reason: 'Only a suffix was observed; inspect history.' });
    await expect(result).rejects.toMatchObject({ deliveryStatus: 'unknown', message: 'Only a suffix was observed; inspect history.' });
    expect(socket.sent.filter(msg => msg.type === 'submit')).toHaveLength(1);
  });

  it('does not relabel a missing acknowledgement as unsent or replay the message', async () => {
    socket.open();
    const result = expect(submitSessionMessage(url, 'legacy', 'whole message')).rejects.toMatchObject({ deliveryStatus: 'unknown' });
    await vi.advanceTimersByTimeAsync(10_000);
    await result;
    expect(socket.sent.filter(msg => msg.type === 'submit')).toHaveLength(1);
  });

  it('accepts an acknowledgement backed by complete provider text', async () => {
    socket.open();
    const result = submitSessionMessage(url, 'legacy', 'whole message');
    const request = socket.sent.find(msg => msg.type === 'submit')!;
    socket.reply({ type: 'submitAck', requestId: request.requestId, sessionId: 'legacy', ok: true });
    await expect(result).resolves.toBeUndefined();
  });
});
