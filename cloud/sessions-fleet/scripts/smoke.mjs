#!/usr/bin/env node

import { createHash, generateKeyPairSync, randomBytes, randomUUID, sign } from 'node:crypto';
import { createInterface } from 'node:readline/promises';

const baseURL = (process.env.SESSIONS_FLEET_SMOKE_URL || 'http://127.0.0.1:5173').replace(/\/$/, '');
const email = process.env.SESSIONS_FLEET_SMOKE_EMAIL;
if (!email) throw new Error('set SESSIONS_FLEET_SMOKE_EMAIL to a disposable test address');

let access = '';
let refresh = '';
let session = '';
const machineID = randomUUID();
const { publicKey, privateKey } = generateKeyPairSync('ed25519');
const rawPublic = publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64url');

async function request(path, { method = 'GET', body, auth = false, signed = false } = {}) {
  const bodyText = body === undefined ? '' : JSON.stringify(body);
  const headers = new Headers();
  if (bodyText) headers.set('content-type', 'application/json');
  if (auth) {
    headers.set('authorization', `Bearer ${access}`);
    headers.set('x-refresh-token', refresh);
  }
  if (signed) signHeaders(headers, machineID, method, path, bodyText);
  const response = await fetch(baseURL + path, { method, headers, body: bodyText || undefined });
  rotate(response.headers);
  const result = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${result.error || response.statusText}`);
  return result;
}

function signHeaders(headers, id, method, path, body) {
  const timestamp = String(Math.floor(Date.now() / 1000));
  const nonce = randomBytes(18).toString('base64url');
  const hash = createHash('sha256').update(body).digest('hex');
  const signature = sign(null, Buffer.from(id + timestamp + nonce + method + path + hash), privateKey);
  headers.set('x-sessions-machine-id', id);
  headers.set('x-sessions-timestamp', timestamp);
  headers.set('x-sessions-nonce', nonce);
  headers.set('x-sessions-signature', signature.toString('base64url'));
}

function rotate(headers) {
  const nextAccess = headers.get('x-new-access-token');
  const nextRefresh = headers.get('x-new-refresh-token');
  if (nextAccess && nextRefresh) [access, refresh] = [nextAccess, nextRefresh];
}

await request('/api/auth-token/magic-link', { method: 'POST', body: { email } });
const terminal = createInterface({ input: process.stdin, output: process.stderr });
const token = process.env.SESSIONS_FLEET_SMOKE_CODE || await terminal.question(`Magic-link code/token sent to ${email}: `);
terminal.close();
const login = await request('/api/auth-token/magic-link/verify', { method: 'POST', body: { token: token.trim() } });
({ token: access, refresh_token: refresh, session_token: session } = login);
if (!access || !refresh) throw new Error('magic-link verification returned no token pair');

const registration = {
  machine_id: machineID,
  name: 'sessions-fleet smoke',
  machine_public_key: rawPublic,
  endpoints_json: { lan: 'http://192.0.2.1:8787', tailnet: 'https://smoke.example.ts.net' },
  daemon_version: 'smoke',
};
await request('/api/machines/register', { method: 'POST', body: registration, auth: true, signed: true });
await request('/api/machines/heartbeat', { method: 'POST', body: { machine_id: machineID }, auth: true, signed: true });
const index = await request('/api/machines/index', { auth: true, signed: true });
if (!index.machines?.some((machine) => machine.id === machineID)) throw new Error('registered machine missing from index');
const found = await request(`/api/machines/${machineID}`, { auth: true, signed: true });
if (found.machine?.machine_public_key !== rawPublic) throw new Error('machine public key did not round-trip');
await request(`/api/machines/${machineID}`, { method: 'DELETE', auth: true, signed: true });
await request('/api/auth-token/logout', { method: 'POST', body: { session_token: session }, auth: true });
console.log('sessions-fleet smoke: ok');
