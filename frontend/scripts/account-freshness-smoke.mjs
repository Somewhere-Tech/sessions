import assert from 'node:assert/strict';

// What the Accounts page says after a provider check. A check that found the
// home signed out, on another kind of login, or could not read it, makes any
// earlier identity history: it never titles the row, never reads as Verified,
// and an older usage reading of that home no longer counts as the account's.
// accountRollup.ts has only type imports, so Node 24 loads it directly.
const rollup = await import(new URL('../src/lib/accountRollup.ts', import.meta.url).href);
const { placementCheck, placementIdentity, rollUpAccounts, groupTitle, freshestReading } = rollup;

const HOUR = 60 * 60_000;
const now = Date.now();
const alice = { email: 'alice@example.test', plan: 'max', checked_at: now - 3 * HOUR };
const profile = (fields) => ({ tool: 'claude', name: 'work', path: '', signed_in: true, sessions: [], last_used: 0, ...fields });
const placement = (fields, usage) => ({ serverId: 'local', machineName: 'This Mac', profile: profile(fields), usage });
const machine = (profiles, usage = []) => ({ serverId: 'local', machineName: 'This Mac', profiles, usage });

// A sidecar from before last_check still reads as the identity it recorded.
const legacy = placement({ identity: alice });
assert.equal(placementCheck(legacy).kind, 'verified');
assert.equal(groupTitle(rollUpAccounts([machine([legacy.profile])])[0]), 'alice@example.test');
assert.equal(placementCheck(placement({ identity: alice, last_check: { at: alice.checked_at, outcome: 'verified' } })).kind, 'verified');

for (const outcome of ['signed_out', 'not_subscription', 'failed']) {
  const checked = placement({ last_check: { at: now - HOUR, outcome }, previous_identity: alice });
  const check = placementCheck(checked);
  assert.equal(check.kind, outcome, `${outcome} is shown as itself`);
  assert.equal(check.previous?.email, 'alice@example.test', 'the old identity is kept as history');
  assert.equal(placementIdentity(checked), undefined, `${outcome}: no current identity`);
  const [group] = rollUpAccounts([machine([checked.profile])]);
  assert.equal(group.identity, undefined);
  assert.equal(groupTitle(group), 'Claude account', `${outcome}: the old email does not title the row`);
  assert.equal(groupTitle(rollUpAccounts([machine([{ ...checked.profile, label: 'Work' }])])[0]), 'Work', 'a nickname still leads');
}

// A forgotten name added again has not been checked since, whatever came before.
const readded = placement({ last_check: { at: now - HOUR, outcome: 'not_checked' }, previous_identity: alice });
assert.equal(placementCheck(readded).kind, 'unchecked');
assert.equal(placementCheck(readded).previous?.email, 'alice@example.test');

// Partial usage: a Codex reading taken before a newer signed-out check is not
// this account's any more; a reading after it is a real provider answer again.
const team = (at) => ({ account_id: 'ws-team', email: 'team@example.test', plan: 'team', checked_at: at });
const reading = (at) => ({
  tool: 'codex', name: 'team', state: 'available', checked_at: at, read_at: at, identity: team(at),
  buckets: [{ limit_id: 'codex', windows: [{ kind: 'primary', used_percent: 40 }] }]
});
const codexOut = { tool: 'codex', name: 'team', last_check: { at: now - HOUR, outcome: 'signed_out' }, previous_identity: team(now - 5 * HOUR) };
const olderRead = placement(codexOut, reading(now - 2 * HOUR));
assert.equal(placementCheck(olderRead).kind, 'signed_out', 'a newer sign-out wins over an older read');
assert.equal(placementIdentity(olderRead), undefined, 'the older read does not restore an identity');
assert.equal(freshestReading([olderRead]), undefined, 'the older reading is not shown as the allowance');
const [ungrouped] = rollUpAccounts([machine([olderRead.profile], [olderRead.usage])]);
assert.equal(ungrouped.matchedByProvider, false, 'a superseded provider ID does not merge homes');
assert.equal(ungrouped.reading, undefined);
const newerRead = placement(codexOut, reading(now - 60_000));
assert.equal(placementCheck(newerRead).kind, 'signed_in', 'a newer provider read wins');
assert.equal(placementIdentity(newerRead)?.email, 'team@example.test');
assert.equal(freshestReading([newerRead])?.usage.read_at, now - 60_000);

// A failed check is unknown; a newer read that found the home signed out is not.
const failedThenOut = placement({ last_check: { at: now - 2 * HOUR, outcome: 'failed' } },
  { tool: 'claude', name: 'work', state: 'signed_out', checked_at: now - HOUR, message: 'Signed out' });
assert.equal(placementCheck(failedThenOut).kind, 'signed_out');
// A verified check followed by a successful read stays signed in.
assert.equal(placementCheck(placement({ identity: alice, last_check: { at: alice.checked_at, outcome: 'verified' } },
  { ...reading(now), tool: 'claude', name: 'work' })).kind, 'signed_in');

// Coordinator counterexamples. A successful recheck is newer than an older
// signed-out usage answer, and an older available read does not demote it.
const recheck = { identity: { email: 'new@example.test', checked_at: 200 }, last_check: { at: 200, outcome: 'verified' } };
const olderOut = placement(recheck, { tool: 'claude', name: 'work', state: 'signed_out', checked_at: 100 });
assert.deepEqual([placementCheck(olderOut).kind, placementCheck(olderOut).at], ['verified', 200]);
assert.equal(placementIdentity(olderOut)?.email, 'new@example.test');
const olderIn = placement(recheck, { tool: 'claude', name: 'work', state: 'available', checked_at: 100 });
assert.deepEqual([placementCheck(olderIn).kind, placementCheck(olderIn).at], ['verified', 200]);
// A negative check and a cached positive reading at the same time: the check wins
// everywhere, so the status, identity, title, grouping and allowance agree.
const tie = placement(codexOut, { ...reading(200) });
tie.profile = { ...tie.profile, last_check: { at: 200, outcome: 'signed_out' } };
assert.equal(placementCheck(tie).kind, 'signed_out');
assert.equal(placementIdentity(tie), undefined);
assert.equal(freshestReading([tie]), undefined);
const [tieGroup] = rollUpAccounts([machine([tie.profile], [tie.usage])]);
assert.deepEqual([tieGroup.matchedByProvider, groupTitle(tieGroup), tieGroup.reading], [false, 'ChatGPT account', undefined]);
// A later read is newer provider evidence and still wins.
const later = placement({ ...codexOut, last_check: { at: 200, outcome: 'signed_out' } }, reading(201));
assert.equal(placementCheck(later).kind, 'signed_in');
assert.equal(placementIdentity(later)?.email, 'team@example.test');
// An undated signed-out read is the oldest evidence, never a newer contradiction.
const undated = placement({ identity: alice }, { tool: 'claude', name: 'work', state: 'signed_out' });
assert.equal(placementCheck(undated).kind, 'verified');
const undatedOnly = placement({}, { tool: 'claude', name: 'work', state: 'signed_out', message: 'Signed out' });
assert.deepEqual([placementCheck(undatedOnly).kind, placementCheck(undatedOnly).at], ['signed_out', undefined]);
// An unavailable read that still reported who is signed in is identity evidence.
const unavailable = placement({ ...codexOut, last_check: { at: 100, outcome: 'failed' } },
  { tool: 'codex', name: 'team', state: 'unavailable', checked_at: 150, identity: team(150) });
assert.equal(placementCheck(unavailable).kind, 'verified');
assert.equal(placementIdentity(unavailable)?.email, 'team@example.test');

// Every combination agrees with itself: no current identity, title or reading
// survives a status that is not positive, and a positive status keeps the
// identity its observation reported.
const outcomes = [undefined, 'verified', 'signed_out', 'not_subscription', 'failed', 'not_checked'];
const usages = [undefined, 'available', 'signed_out', 'unavailable'];
for (const savedAt of [100, 200, 300]) for (const outcome of outcomes) for (const readAt of [100, 200, 300]) for (const state of usages) {
  const saved = outcome === undefined ? {} : outcome === 'verified'
    ? { identity: team(savedAt), last_check: { at: savedAt, outcome } }
    : { last_check: { at: savedAt, outcome }, previous_identity: team(10) };
  const usage = state === undefined ? undefined : state === 'available' ? reading(readAt)
    : state === 'signed_out' ? { tool: 'codex', name: 'team', state, checked_at: readAt }
    : { tool: 'codex', name: 'team', state, checked_at: readAt, identity: team(readAt) };
  const item = placement({ tool: 'codex', name: 'team', ...saved }, usage);
  const check = placementCheck(item);
  const label = `${outcome}@${savedAt} + ${state}@${readAt}: ${check.kind}`;
  const isPositive = check.kind === 'signed_in' || check.kind === 'verified';
  const [group] = rollUpAccounts([machine([item.profile], usage ? [usage] : [])]);
  if (isPositive) {
    assert.equal(placementIdentity(item)?.email, 'team@example.test', label);
    assert.equal(groupTitle(group), 'team@example.test', label);
  } else {
    assert.equal(placementIdentity(item), undefined, label);
    assert.equal(groupTitle(group), 'ChatGPT account', label);
    assert.equal(group.matchedByProvider, false, label);
    if (check.at !== undefined) assert.ok(!group.reading || group.reading.usage.read_at > check.at, label);
  }
  if (outcome && outcome !== 'verified' && usage?.read_at !== undefined && usage.read_at <= savedAt) assert.equal(group.reading, undefined, label);
}

// Split timestamps: a Codex usage read asks who is signed in (identity at 100)
// and reads the limits later (checked_at/read_at 300). A check at 200 that did
// not confirm the sign-in is a barrier the later limits answer cannot cross.
const split = (identityAt, readAt, identity = team(identityAt)) => ({
  tool: 'codex', name: 'team', state: 'available', checked_at: readAt, read_at: readAt, identity,
  buckets: [{ limit_id: 'codex', windows: [{ kind: 'primary', used_percent: 40 }] }]
});
for (const outcome of ['signed_out', 'not_subscription', 'failed', 'not_checked']) {
  const barrier = { tool: 'codex', name: 'team', last_check: { at: 200, outcome }, previous_identity: team(50) };
  const item = placement(barrier, split(100, 300));
  const check = placementCheck(item);
  assert.equal(check.kind === 'signed_in' || check.kind === 'verified', false, `${outcome}: status ${check.kind}`);
  assert.equal(check.at, 200, `${outcome}: the barrier is the newest evidence`);
  assert.equal(placementIdentity(item), undefined, `${outcome}: no resurrected identity`);
  assert.equal(freshestReading([item]), undefined, `${outcome}: no allowance`);
  const [group] = rollUpAccounts([machine([item.profile], [item.usage])]);
  assert.deepEqual([groupTitle(group), group.matchedByProvider, group.identity, group.reading], ['ChatGPT account', false, undefined, undefined], outcome);
  // A newer identity observation does restore it, provider ID and allowance included.
  const restored = placement(barrier, split(250, 300));
  assert.equal(placementCheck(restored).kind, 'signed_in');
  assert.equal(placementIdentity(restored)?.account_id, 'ws-team');
  assert.equal(freshestReading([restored])?.usage.read_at, 300);
  const [restoredGroup] = rollUpAccounts([machine([restored.profile], [restored.usage])]);
  assert.deepEqual([groupTitle(restoredGroup), restoredGroup.matchedByProvider], ['team@example.test', true]);
}

// A reading taken under an identity known to differ from the one this home is
// verified as now is not shown as that account's allowance.
const bobNow = { tool: 'codex', name: 'team', identity: { email: 'bob@example.test', checked_at: 200 }, last_check: { at: 200, outcome: 'verified' } };
const aliceRead = placement(bobNow, split(100, 300, { email: 'alice@example.test', checked_at: 100 }));
assert.equal(placementCheck(aliceRead).kind, 'verified');
assert.equal(placementIdentity(aliceRead)?.email, 'bob@example.test');
assert.equal(freshestReading([aliceRead]), undefined, 'Alice\'s reading is not Bob\'s allowance');
const [bobGroup] = rollUpAccounts([machine([aliceRead.profile], [aliceRead.usage])]);
assert.deepEqual([groupTitle(bobGroup), bobGroup.reading], ['bob@example.test', undefined]);
// A newer read as Alice is newer identity evidence: the row becomes Alice, with her reading.
const aliceNewer = placement(bobNow, split(250, 300, { email: 'alice@example.test', checked_at: 250 }));
assert.equal(placementIdentity(aliceNewer)?.email, 'alice@example.test');
assert.equal(freshestReading([aliceNewer])?.usage.identity.email, 'alice@example.test');
// Different provider IDs are a known difference even with one email.
const idBob = { ...bobNow, identity: { account_id: 'ws-b', email: 'me@example.test', checked_at: 200 } };
assert.equal(freshestReading([placement(idBob, split(100, 300, { account_id: 'ws-a', email: 'me@example.test', checked_at: 100 }))]), undefined);
// The same email without a provider ID is no known difference: the reading stays
// this home's, and nothing is merged across computers on the email alone.
const sameEmail = { ...bobNow, identity: { email: 'me@example.test', checked_at: 200 } };
const sameEmailRead = placement(sameEmail, split(100, 300, { email: 'me@example.test', checked_at: 100 }));
assert.equal(freshestReading([sameEmailRead])?.usage.read_at, 300);
const homes = rollUpAccounts([machine([sameEmailRead.profile], [sameEmailRead.usage]),
  { serverId: 'mini', machineName: 'Mac mini', profiles: [sameEmailRead.profile], usage: [sameEmailRead.usage] }]);
assert.equal(homes.length, 2, 'email alone never merges homes');
// A reading that reported no identity is attributed unless a barrier is newer.
const noIdentity = { ...split(0, 300), identity: undefined };
assert.equal(freshestReading([placement(bobNow, noIdentity)])?.usage.read_at, 300);
assert.equal(freshestReading([placement({ tool: 'codex', name: 'team', last_check: { at: 300, outcome: 'failed' } }, noIdentity)]), undefined);
// A stale reading kept from an earlier read follows the same barrier.
const staleRead = { ...split(250, 300), state: 'unavailable', stale: true, read_at: 150 };
assert.equal(freshestReading([placement({ tool: 'codex', name: 'team', last_check: { at: 200, outcome: 'failed' } }, staleRead)]), undefined);
assert.equal(freshestReading([placement({ tool: 'codex', name: 'team', identity: team(100) }, staleRead)])?.usage.read_at, 150);
// Legacy: an identity with no last_check, and a matching newer read, behave as before.
const legacyRead = placement({ tool: 'codex', name: 'team', identity: team(100) }, split(300, 300));
assert.deepEqual([placementCheck(legacyRead).kind, placementIdentity(legacyRead)?.checked_at, freshestReading([legacyRead])?.usage.read_at], ['signed_in', 300, 300]);

console.log('account freshness: negative and failed checks make old identities history, re-add is unchecked, legacy reads verified, older usage readings drop out, freshest evidence wins with conservative ties, split-time identity barriers and known-mismatch readings hold');
