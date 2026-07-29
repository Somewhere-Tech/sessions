// Wire protocol between `prettyd` (the router) and each session-runner
// (the long-lived process that owns one PTY + Unix socket).
//
// Each frame on the socket is:
//   [ 4-byte big-endian payload length ][ 1-byte type ][ payload ]
//
// Length excludes the length field itself but includes the type byte.
// Maximum payload length is bounded by MAX_FRAME_LEN to keep a single
// dropped/garbled frame from forcing an OOM read.

import { Buffer } from 'node:buffer';

export const MAX_FRAME_LEN = 4 * 1024 * 1024;

// Incremented when the runner↔daemon wire protocol gains new mandatory fields.
// The daemon reads hello.protocolVersion ?? 0 and logs a warning on mismatch
// but ALWAYS attaches — live runners on old code must never be dropped.
export const RUNNER_PROTOCOL_VERSION = 1;

export const enum FrameType {
  // Runner → client (prettyd) ----------------------------------------------
  HELLO          = 0x20, // sent on connect; payload = JSON RunnerHello
  OUTPUT         = 0x21, // payload = 4-byte BE seq + utf8 chunk
  EXIT           = 0x22, // payload = JSON {code, signal, seq}
  SNAPSHOT_RES   = 0x23, // payload = utf8 serialized xterm buffer
  REPLAY_DONE    = 0x24, // payload = empty; signals end of replay stream
  // Client (prettyd) → runner ----------------------------------------------
  INPUT          = 0x10, // payload = utf8 input bytes
  RESIZE         = 0x11, // payload = JSON {cols, rows}
  SNAPSHOT_REQ   = 0x12, // payload = empty
  REPLAY_REQ     = 0x13, // payload = 4-byte BE afterSeq
  KILL           = 0x14  // payload = empty; ask runner to terminate the PTY
}

export interface RunnerHello {
  id: string;
  cmd: string;
  args: string[];
  cwd: string;
  cols: number;
  rows: number;
  createdAt: number;
  pid: number;
  currentSeq: number;
  // Added in protocol v1. Absent on runners built before this field was
  // introduced (treat missing as 0 / legacy). See RUNNER_PROTOCOL_VERSION.
  protocolVersion?: number;
}

export interface RunnerExit {
  code: number | null;
  signal: string | null;
  seq: number;
}

// Encode a frame. `payload` may be a Buffer, string (utf8), or undefined.
export function encodeFrame(type: FrameType, payload?: Buffer | string): Buffer {
  const body = payload === undefined
    ? Buffer.alloc(0)
    : (typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload);
  if (body.length + 1 > MAX_FRAME_LEN) {
    throw new Error(`runner frame too large: ${body.length + 1} > ${MAX_FRAME_LEN}`);
  }
  const out = Buffer.allocUnsafe(4 + 1 + body.length);
  out.writeUInt32BE(body.length + 1, 0);
  out.writeUInt8(type, 4);
  body.copy(out, 5);
  return out;
}

// Encode an OUTPUT frame: seq (4 bytes BE) followed by utf8 chunk.
export function encodeOutput(seq: number, chunk: string): Buffer {
  const data = Buffer.from(chunk, 'utf8');
  const payload = Buffer.allocUnsafe(4 + data.length);
  payload.writeUInt32BE(seq >>> 0, 0);
  data.copy(payload, 4);
  return encodeFrame(FrameType.OUTPUT, payload);
}

export function decodeOutput(payload: Buffer): { seq: number; data: string } {
  if (payload.length < 4) throw new Error('OUTPUT frame too short');
  const seq = payload.readUInt32BE(0);
  const data = payload.slice(4).toString('utf8');
  return { seq, data };
}

// Streaming frame parser. Feed it socket chunks; it emits whole frames.
// Buffers partials internally so a frame split across multiple `data`
// events is reassembled correctly.
export class FrameParser {
  private buf: Buffer = Buffer.alloc(0);

  push(chunk: Buffer, onFrame: (type: FrameType, payload: Buffer) => void): void {
    this.buf = this.buf.length === 0 ? chunk : Buffer.concat([this.buf, chunk]);
    while (this.buf.length >= 4) {
      const len = this.buf.readUInt32BE(0);
      if (len > MAX_FRAME_LEN || len < 1) {
        // Stream desync — drop everything to avoid pinning memory.
        this.buf = Buffer.alloc(0);
        throw new Error(`bad frame length ${len}`);
      }
      if (this.buf.length < 4 + len) return; // wait for more
      const type = this.buf.readUInt8(4) as FrameType;
      const payload = Buffer.from(this.buf.slice(5, 4 + len)); // copy out
      this.buf = this.buf.slice(4 + len);
      onFrame(type, payload);
    }
  }
}
