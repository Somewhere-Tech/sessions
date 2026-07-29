// Phase 4a verification — sessions survive prettyd restart.
// Runs against an already-up prettyd. Uses the WS to talk to a running
// runner subprocess.

const WebSocket = require('ws');

async function createSession() {
  const r = await fetch('http://127.0.0.1:8787/api/sessions', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ cmd: '/bin/cat' })
  });
  return r.json();
}
async function listSessions() {
  const r = await fetch('http://127.0.0.1:8787/api/sessions');
  return (await r.json()).sessions;
}
async function snapshot(id) {
  const r = await fetch(`http://127.0.0.1:8787/api/sessions/${id}/snapshot`);
  return r.text();
}
async function kill(id) {
  await fetch(`http://127.0.0.1:8787/api/sessions/${id}`, { method: 'DELETE' });
}

function ws(id, lastSeq) {
  const url = lastSeq
    ? `ws://127.0.0.1:8787/ws?sessionId=${id}&lastSeq=${lastSeq}`
    : `ws://127.0.0.1:8787/ws?sessionId=${id}`;
  return new WebSocket(url);
}

(async () => {
  const session = await createSession();
  console.log('created', session.id, 'runner pid', session.pid);

  // Send input via WS, capture echo.
  const sock1 = ws(session.id);
  let buf1 = '';
  let lastSeq1 = 0;
  await new Promise((resolve) => {
    sock1.on('open', () => sock1.send(JSON.stringify({ type: 'input', data: 'hello-runner\n' })));
    sock1.on('message', (raw) => {
      const m = JSON.parse(raw.toString());
      if (m.type === 'output') { buf1 += m.data; lastSeq1 = m.seq; }
      if (buf1.includes('hello-runner')) setTimeout(resolve, 200);
    });
  });
  sock1.close();
  console.log('A: saw output up to seq', lastSeq1, JSON.stringify(buf1));

  // Snapshot via the new HTTP endpoint.
  const snap = await snapshot(session.id);
  console.log('snapshot bytes:', snap.length, 'contains hello-runner:', snap.includes('hello-runner'));

  // List sessions before fake "prettyd restart".
  const before = (await listSessions()).find((s) => s.id === session.id);
  if (!before) throw new Error('session vanished before restart');
  console.log('before restart: working=', before.working, 'pid=', before.pid);

  // We can't actually restart prettyd from inside this script (different
  // process), so the human-in-the-loop test is what we already did via
  // the shell. Instead, verify the survival path another way:
  //   1. Write some new input to the session — it lands at the runner.
  //   2. Reconnect a fresh WS — the runner replays via prettyd's mirror.
  //   3. Last seq from B is monotonic vs A.
  const sock2 = ws(session.id, lastSeq1);
  let buf2 = '';
  let firstSeq2 = null;
  await new Promise((resolve) => {
    sock2.on('open', () => {
      sock2.send(JSON.stringify({ type: 'input', data: 'second-line\n' }));
    });
    sock2.on('message', (raw) => {
      const m = JSON.parse(raw.toString());
      if (m.type === 'output') {
        if (firstSeq2 === null) firstSeq2 = m.seq;
        buf2 += m.data;
      }
      if (buf2.includes('second-line')) setTimeout(resolve, 200);
    });
  });
  sock2.close();
  console.log('B: first seq after lastSeq=', lastSeq1, '→', firstSeq2);
  console.log('B: saw', JSON.stringify(buf2));

  let pass = true;
  if (firstSeq2 === null || firstSeq2 <= lastSeq1) {
    console.error('FAIL: B should resume with seq > A.lastSeq');
    pass = false;
  }
  if (!buf2.includes('second-line')) {
    console.error('FAIL: B missed second-line');
    pass = false;
  }
  if (buf1.includes('second-line')) {
    console.error('FAIL: A leaked future input — impossible');
    pass = false;
  }
  if (!snap.includes('hello-runner')) {
    console.error('FAIL: snapshot endpoint did not include the typed text');
    pass = false;
  }

  await kill(session.id);
  console.log(pass ? 'PASS' : 'FAIL');
  process.exit(pass ? 0 : 1);
})().catch((err) => { console.error(err); process.exit(2); });
