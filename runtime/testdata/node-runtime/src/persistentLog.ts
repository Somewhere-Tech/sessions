// Append-only on-disk mirror of the runner's EventLog. Survives runner
// restart (and therefore Mac reboot, which is the actual point — the
// in-memory ring buffer dies with the process).
//
// File format — a sequence of records, each:
//   [4-byte BE record length L]
//   [4-byte BE seq]
//   [L-4 bytes utf8 payload]
// Record length is the count of bytes that follow the length field
// itself (i.e. seq + payload). Reader iterates by reading the length
// field, then the record body, until EOF.
//
// Cap: SOFT_CAP_BYTES default 16MB. Whenever a write pushes us past the
// cap, we synchronously rewrite the file keeping only the most recent
// half (TARGET_AFTER_TRIM_BYTES). This is amortized cheap — we trim at
// most once per cap-doubling worth of output.
//
// Crash safety: each append is fs.writeSync (no internal buffer), so a
// hard crash loses at most the chunk currently being assembled by the
// PTY (microseconds). The trim is `write to <path>.tmp` then
// `rename(<path>.tmp, <path>)` — atomic on POSIX.

import fs from 'node:fs';
import path from 'node:path';

const SOFT_CAP_BYTES = 16 * 1024 * 1024;
const TARGET_AFTER_TRIM_BYTES = 8 * 1024 * 1024;
const RECORD_HEADER_BYTES = 4;       // length prefix
const RECORD_FIXED_BYTES = 4;        // seq

export interface PersistedEvent {
  seq: number;
  data: string;
}

export class PersistentLog {
  private fd: number;
  private bytesOnDisk: number;
  private trimming = false;
  // Batched write buffer: append() pushes a pre-encoded frame here and
  // schedules a setImmediate flush instead of writing on every PTY chunk
  // (which would serialise every keystroke on a writeSync call).
  // flush() drains ALL pending frames in ONE writeSync call, so there is
  // never an in-flight async write that close() cannot recover.  This is
  // the key difference from the previous fs.write() design: SIGTERM during
  // an async write could lose the in-flight batch even though close() called
  // flushSync(), because the batch had already been dequeued from `pending`.
  // With writeSync the flush is atomic from JS's perspective: pending is
  // drained and written before we return. close() therefore just calls
  // flush() before setting closed=true and the data is guaranteed on disk.
  private pending: Buffer[] = [];
  private flushScheduled = false;
  private closed = false;
  // Cumulative metrics surfaced via /api/health/deep for debuggability.
  private appendCount = 0;
  private droppedFrames = 0;

  private constructor(public readonly filePath: string, fd: number, bytesOnDisk: number) {
    this.fd = fd;
    this.bytesOnDisk = bytesOnDisk;
  }

  stats(): { bytesOnDisk: number; pending: number; appendCount: number; droppedFrames: number } {
    let pendingBytes = 0;
    for (const b of this.pending) pendingBytes += b.length;
    return { bytesOnDisk: this.bytesOnDisk, pending: pendingBytes, appendCount: this.appendCount, droppedFrames: this.droppedFrames };
  }

  static open(filePath: string): PersistentLog {
    fs.mkdirSync(path.dirname(filePath), { recursive: true, mode: 0o700 });
    // Remove any leftover .tmp from a previous interrupted atomic rename.
    // If trim() was killed between writeFileSync and renameSync (e.g. a
    // hard power-off), the stale .tmp would linger on disk forever.
    try { fs.unlinkSync(filePath + '.tmp'); } catch { /* not present */ }
    // Open in append+read mode so we can both append AND truncate-rewrite.
    const fd = fs.openSync(filePath, 'a+');
    const stat = fs.fstatSync(fd);
    return new PersistentLog(filePath, fd, stat.size);
  }

  // Read the whole file from disk, decoding records. Used at runner
  // startup to rebuild the in-memory state.
  static restoreFrom(filePath: string): PersistedEvent[] {
    if (!fs.existsSync(filePath)) return [];
    const buf = fs.readFileSync(filePath);
    const out: PersistedEvent[] = [];
    let off = 0;
    while (off + RECORD_HEADER_BYTES <= buf.length) {
      const recLen = buf.readUInt32BE(off);
      off += RECORD_HEADER_BYTES;
      if (recLen < RECORD_FIXED_BYTES || off + recLen > buf.length) {
        // Truncated tail — likely a crash mid-write. Stop here.
        break;
      }
      const seq = buf.readUInt32BE(off);
      const data = buf.slice(off + RECORD_FIXED_BYTES, off + recLen).toString('utf8');
      off += recLen;
      out.push({ seq, data });
    }
    return out;
  }

  append(seq: number, data: string): void {
    if (this.closed) return;
    const payload = Buffer.from(data, 'utf8');
    const recLen = RECORD_FIXED_BYTES + payload.length;
    const frame = Buffer.allocUnsafe(RECORD_HEADER_BYTES + recLen);
    frame.writeUInt32BE(recLen, 0);
    frame.writeUInt32BE(seq >>> 0, RECORD_HEADER_BYTES);
    payload.copy(frame, RECORD_HEADER_BYTES + RECORD_FIXED_BYTES);
    this.appendCount++;
    this.pending.push(frame);
    this.scheduleFlush();
  }

  private scheduleFlush(): void {
    if (this.flushScheduled || this.closed) return;
    this.flushScheduled = true;
    setImmediate(() => { this.flushScheduled = false; this.flush(); });
  }

  // Drain ALL pending frames in one writeSync call. Because the write is
  // synchronous there is never an in-flight batch that close() cannot see.
  // trim (rare, only when the soft cap is crossed) runs inline here, off
  // the per-chunk hot path.
  private flush(): void {
    if (this.trimming || this.pending.length === 0 || this.closed) return;
    const batch = this.pending.length === 1 ? this.pending[0]! : Buffer.concat(this.pending);
    this.pending = [];
    try {
      fs.writeSync(this.fd, batch, 0, batch.length);
      this.bytesOnDisk += batch.length;
      if (this.bytesOnDisk > SOFT_CAP_BYTES && !this.trimming) this.trim();
    } catch {
      // Write failed (disk full, fd gone) — drop this batch rather than
      // stall the runner. Counted for /health/deep visibility.
      this.droppedFrames++;
    }
    if (this.pending.length > 0) this.scheduleFlush();
  }

  // Trim from the front: keep just the most recent
  // TARGET_AFTER_TRIM_BYTES of records. We re-read the file in record
  // boundaries so we never split a record. Atomic via rename.
  private trim(): void {
    this.trimming = true;
    try {
      const all = fs.readFileSync(this.filePath);
      // Walk records to find the byte offset of the first record we'll keep.
      // We want to keep the last TARGET_AFTER_TRIM_BYTES; find the earliest
      // record that fits within that window.
      const cutoffSize = TARGET_AFTER_TRIM_BYTES;
      const recordOffsets: number[] = [];
      let off = 0;
      while (off + RECORD_HEADER_BYTES <= all.length) {
        const recLen = all.readUInt32BE(off);
        if (recLen < RECORD_FIXED_BYTES || off + RECORD_HEADER_BYTES + recLen > all.length) break;
        recordOffsets.push(off);
        off += RECORD_HEADER_BYTES + recLen;
      }
      const totalEnd = off; // one past the last complete record
      // Find the earliest record offset where keeping from there to end
      // is within the target size.  Use -1 as "not found yet" so we can
      // distinguish "first record at offset 0 fit" from "nothing fit".
      let firstKept = -1;
      for (const o of recordOffsets) {
        if (totalEnd - o <= cutoffSize) {
          firstKept = o;
          break;
        }
      }
      if (firstKept < 0) {
        // No record individually fits within TARGET_AFTER_TRIM_BYTES (e.g.
        // a single > 8 MB chunk).  Force-keep only the last record so trim
        // always makes progress and doesn't re-fire on every subsequent flush.
        firstKept = recordOffsets.length > 0 ? recordOffsets[recordOffsets.length - 1]! : 0;
      }
      const trimmed = all.slice(firstKept, totalEnd);
      const tmpPath = this.filePath + '.tmp';
      fs.writeFileSync(tmpPath, trimmed, { mode: 0o600 });
      fs.renameSync(tmpPath, this.filePath);
      // Reopen — the old fd points at the unlinked inode after rename.
      try { fs.closeSync(this.fd); } catch { /* ignore */ }
      this.fd = fs.openSync(this.filePath, 'a+');
      this.bytesOnDisk = trimmed.length;
    } finally {
      this.trimming = false;
    }
  }

  close(): void {
    if (this.closed) return;
    // Flush BEFORE setting closed so flush()'s guard doesn't short-circuit.
    // flush() is now synchronous, so this is loss-free on SIGTERM.
    this.flush();
    this.closed = true;
    try { fs.closeSync(this.fd); } catch { /* ignore */ }
  }

  // Used by tests + the runner's exit-cleanup path.
  unlink(): void {
    this.close();
    try { fs.unlinkSync(this.filePath); } catch { /* ignore */ }
  }
}
