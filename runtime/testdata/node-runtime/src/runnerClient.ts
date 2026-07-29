// prettyd-side wrapper around a Unix socket connection to a runner.
// Encapsulates the framing protocol; presents a typed event-emitter API.
//
// One RunnerClient per session. prettyd creates it on session create
// (after spawning the runner) AND on startup discovery (when an existing
// runner is found in the state dir).

import { connect, type Socket } from 'node:net';
import { EventEmitter } from 'node:events';
import {
  FrameParser, FrameType, encodeFrame, decodeOutput,
  type RunnerHello, type RunnerExit
} from './runnerProtocol.js';

export interface RunnerOutput {
  seq: number;
  data: string;
}

export interface RunnerClientEvents {
  hello: (h: RunnerHello) => void;
  output: (ev: RunnerOutput) => void;
  exit: (ev: RunnerExit) => void;
  snapshot: (text: string) => void;
  replayDone: () => void;
  disconnect: (err?: Error) => void;
}

export declare interface RunnerClient {
  on<K extends keyof RunnerClientEvents>(ev: K, cb: RunnerClientEvents[K]): this;
  off<K extends keyof RunnerClientEvents>(ev: K, cb: RunnerClientEvents[K]): this;
  emit<K extends keyof RunnerClientEvents>(ev: K, ...args: Parameters<RunnerClientEvents[K]>): boolean;
}

export class RunnerClient extends EventEmitter {
  private sock: Socket | null = null;
  private parser = new FrameParser();
  private connected = false;
  // Outstanding snapshot requests, resolved in arrival order.
  private snapshotQueue: Array<(text: string) => void> = [];
  // Ensures 'disconnect' fires at most once regardless of how many error/close
  // events arrive (both can fire on the same socket failure).
  private disconnected = false;

  constructor(public readonly sockPath: string) { super(); }

  connect(timeoutMs: number = 2000): Promise<RunnerHello> {
    return new Promise((resolve, reject) => {
      const sock = connect(this.sockPath);
      this.sock = sock;
      let helloReceived = false;
      const timer = setTimeout(() => {
        if (!helloReceived) {
          sock.destroy();
          reject(new Error(`runner ${this.sockPath} did not send HELLO within ${timeoutMs}ms`));
        }
      }, timeoutMs);
      const settle = (): void => { clearTimeout(timer); };
      const onError = (err: Error): void => {
        settle();
        if (!helloReceived) reject(err);
        // Don't emit 'disconnect' here — the 'close' event ALWAYS follows
        // an 'error' on a Unix socket, so we emit exactly once from close().
        // Emitting from both handlers causes callers to handle disconnect
        // twice (double cleanup, double 'runner-lost' forwarding, etc.).
      };
      sock.once('error', onError);
      sock.on('data', (chunk: Buffer) => {
        try {
          this.parser.push(chunk, (type, payload) => {
            this.handleFrame(type, payload, (h) => {
              helloReceived = true;
              settle();
              resolve(h);
            });
          });
        } catch (err) {
          sock.destroy();
        }
      });
      sock.on('close', () => {
        this.connected = false;
        if (!this.disconnected) {
          this.disconnected = true;
          // Drain pending snapshot waiters so requestSnapshot() promises
          // never leak.  Resolve with '' — callers treat the empty string
          // as a signal that the snapshot is unavailable.
          const waiters = this.snapshotQueue.splice(0);
          for (const resolve of waiters) resolve('');
          this.emit('disconnect');
        }
      });
      sock.on('connect', () => { this.connected = true; });
    });
  }

  private handleFrame(type: FrameType, payload: Buffer, onHello: (h: RunnerHello) => void): void {
    switch (type) {
      case FrameType.HELLO: {
        const h = JSON.parse(payload.toString('utf8')) as RunnerHello;
        this.emit('hello', h);
        onHello(h);
        return;
      }
      case FrameType.OUTPUT: {
        const ev = decodeOutput(payload);
        this.emit('output', ev);
        return;
      }
      case FrameType.EXIT: {
        const e = JSON.parse(payload.toString('utf8')) as RunnerExit;
        this.emit('exit', e);
        return;
      }
      case FrameType.SNAPSHOT_RES: {
        const text = payload.toString('utf8');
        const waiter = this.snapshotQueue.shift();
        if (waiter) waiter(text);
        else this.emit('snapshot', text);
        return;
      }
      case FrameType.REPLAY_DONE: {
        this.emit('replayDone');
        return;
      }
      default:
        return;
    }
  }

  send(input: string): void {
    if (!this.sock || !this.connected) return;
    this.sock.write(encodeFrame(FrameType.INPUT, input));
  }

  resize(cols: number, rows: number): void {
    if (!this.sock || !this.connected) return;
    this.sock.write(encodeFrame(FrameType.RESIZE, JSON.stringify({ cols, rows })));
  }

  requestSnapshot(): Promise<string> {
    return new Promise((resolve) => {
      this.snapshotQueue.push(resolve);
      if (this.sock && this.connected) {
        this.sock.write(encodeFrame(FrameType.SNAPSHOT_REQ));
      }
    });
  }

  requestReplay(afterSeq: number): void {
    if (!this.sock || !this.connected) return;
    const payload = Buffer.allocUnsafe(4);
    payload.writeUInt32BE(afterSeq >>> 0, 0);
    this.sock.write(encodeFrame(FrameType.REPLAY_REQ, payload));
  }

  kill(): void {
    if (!this.sock || !this.connected) return;
    this.sock.write(encodeFrame(FrameType.KILL));
  }

  disconnect(): void {
    try { this.sock?.end(); } catch { /* ignore */ }
  }

  isConnected(): boolean {
    return this.connected;
  }
}
