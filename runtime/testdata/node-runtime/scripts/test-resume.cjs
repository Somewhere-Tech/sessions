// Phase 2 verification.
// 1. Create a long-lived /bin/cat session.
// 2. Subscribe via WS, send three lines of input, capture echoed output + seqs.
// 3. Close the WS but keep the session alive.
// 4. Send three more lines via a *separate* WS (briefly).
//    [Better: feed input through the original connection before closing,
//     and then verify after reconnect we *only* see what we missed.]
// 5. Reconnect with lastSeq = the last seq we saw, and verify only the
//    missed chunks come back, no duplicates.

const WebSocket = require('ws');

async function createSession() {
  const r = await fetch('http://127.0.0.1:8787/api/sessions', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ cmd: '/bin/cat' })
  });
  return r.json();
}

async function killSession(id) {
  await fetch(`http://127.0.0.1:8787/api/sessions/${id}`, { method: 'DELETE' });
}

function connect(sessionId, lastSeq, onMsg) {
  const url = lastSeq > 0
    ? `ws://127.0.0.1:8787/ws?sessionId=${sessionId}&lastSeq=${lastSeq}`
    : `ws://127.0.0.1:8787/ws?sessionId=${sessionId}`;
  const ws = new WebSocket(url);
  return new Promise((resolve, reject) => {
    ws.on('open', () => resolve(ws));
    ws.on('message', (raw) => {
      try { onMsg(JSON.parse(raw.toString())); } catch { /* ignore */ }
    });
    ws.on('error', reject);
  });
}

function input(ws, data) {
  ws.send(JSON.stringify({ type: 'input', data }));
}

function wait(ms) { return new Promise((r) => setTimeout(r, ms)); }

(async () => {
  const session = await createSession();
  console.log('session', session.id);

  // === First connect ===
  const seenA = []; // {seq, data}
  const wsA = await connect(session.id, 0, (msg) => {
    if (msg.type === 'output') seenA.push({ seq: msg.seq, data: msg.data });
    else if (msg.type === 'hello') console.log('A hello: currentSeq=' + msg.currentSeq + ' resumedFromSeq=' + msg.resumedFromSeq);
    else if (msg.type === 'gap') console.log('A gap', msg);
  });

  input(wsA, 'first\n');
  input(wsA, 'second\n');
  input(wsA, 'third\n');
  await wait(250);

  const lastSeqAtA = seenA.length > 0 ? seenA[seenA.length - 1].seq : 0;
  const seenAStr = seenA.map((e) => e.data).join('');
  console.log('A saw seqs', seenA.map((e) => e.seq).join(','), 'data=' + JSON.stringify(seenAStr));
  console.log('A lastSeq =', lastSeqAtA);

  // === Disconnect, simulate phone lock ===
  wsA.close();
  console.log('--- ws A closed ---');
  await wait(500);

  // While disconnected, push more input. With nothing subscribed, the PTY
  // will still emit the echoed output, and the buffer will record it.
  // We need a way to send input while disconnected — temporarily reconnect,
  // send, then close. (A real client would just push; this simulates the
  // *daemon's* perspective of "data arrived while no WS was attached".)
  const transient = await connect(session.id, lastSeqAtA, (msg) => { /* drain */ });
  input(transient, 'fourth\n');
  input(transient, 'fifth\n');
  await wait(150);
  transient.close();
  await wait(800);

  // === Reconnect with lastSeq from A ===
  const seenB = [];
  const wsB = await connect(session.id, lastSeqAtA, (msg) => {
    if (msg.type === 'output') seenB.push({ seq: msg.seq, data: msg.data });
    else if (msg.type === 'hello') console.log('B hello: currentSeq=' + msg.currentSeq + ' resumedFromSeq=' + msg.resumedFromSeq);
    else if (msg.type === 'gap') console.log('B gap', msg);
  });
  await wait(300);

  const seenBStr = seenB.map((e) => e.data).join('');
  console.log('B saw seqs', seenB.map((e) => e.seq).join(','), 'data=' + JSON.stringify(seenBStr));

  // === Assertions ===
  let pass = true;
  // 1. No duplicates: every B seq must be > lastSeqAtA
  for (const ev of seenB) {
    if (ev.seq <= lastSeqAtA) {
      console.error('FAIL: B contained seq', ev.seq, 'which A already saw (lastSeqAtA=' + lastSeqAtA + ')');
      pass = false;
    }
  }
  // 2. B's text contains "fourth" and "fifth" (echoed each twice, since cat).
  if (!seenBStr.includes('fourth')) { console.error('FAIL: missed "fourth" after reconnect'); pass = false; }
  if (!seenBStr.includes('fifth')) { console.error('FAIL: missed "fifth" after reconnect'); pass = false; }
  // 3. B should NOT contain "first"/"second"/"third" — those are below lastSeqAtA.
  if (seenBStr.includes('first') || seenBStr.includes('second') || seenBStr.includes('third')) {
    console.error('FAIL: replay contained pre-lastSeq data');
    pass = false;
  }

  wsB.close();
  await killSession(session.id);

  console.log(pass ? 'PASS: Phase 2 reconnect + replay works, no duplicates' : 'FAIL');
  process.exit(pass ? 0 : 1);
})().catch((err) => { console.error(err); process.exit(2); });
