// Per-session bounded event log. Each PTY output chunk is captured here
// with a monotonic seq number. The buffer drops oldest events when its
// total byte size exceeds the cap — when a client requests a seq that has
// already aged out, the WS layer emits a `gap` message so the client can
// recover (clear screen, replay from current head).
//
// In-memory only for Phase 2. Phase 4 will mirror this to SQLite so the
// log survives a daemon restart.

export interface OutputEvent {
  seq: number;
  data: string;
  ts: number;
}

const DEFAULT_CAP_BYTES = 4 * 1024 * 1024;

export class EventLog {
  private events: OutputEvent[] = [];
  private bytes = 0;
  private nextSeq = 1;
  private readonly capBytes: number;

  constructor(capBytes: number = DEFAULT_CAP_BYTES) {
    this.capBytes = capBytes;
  }

  push(data: string): OutputEvent {
    return this.pushAt(this.nextSeq, data);
  }

  // Append with a caller-supplied seq number. prettyd's mirror uses this
  // so it inherits the runner's monotonic seq sequence verbatim — that
  // way `since(N)` semantics line up across the prettyd/runner boundary
  // even after a prettyd restart.
  pushAt(seq: number, data: string): OutputEvent {
    const ev: OutputEvent = { seq, data, ts: Date.now() };
    this.events.push(ev);
    this.bytes += Buffer.byteLength(data, 'utf8');
    if (seq >= this.nextSeq) this.nextSeq = seq + 1;
    while (this.bytes > this.capBytes && this.events.length > 1) {
      const dropped = this.events.shift()!;
      this.bytes -= Buffer.byteLength(dropped.data, 'utf8');
    }
    return ev;
  }

  // Returns events with seq > afterSeq, oldest first. If afterSeq is below
  // the buffer's oldest seq, the second tuple entry signals the gap so the
  // caller can warn the client.
  since(afterSeq: number): { events: OutputEvent[]; gap: boolean; oldest: number; current: number } {
    const oldest = this.events.length > 0 ? this.events[0]!.seq : this.nextSeq;
    const current = this.nextSeq - 1;
    if (this.events.length === 0) {
      return { events: [], gap: false, oldest, current };
    }
    const gap = afterSeq + 1 < oldest;
    const events = this.events.filter((e) => e.seq > afterSeq);
    return { events, gap, oldest, current };
  }

  currentSeq(): number {
    return this.nextSeq - 1;
  }
}
