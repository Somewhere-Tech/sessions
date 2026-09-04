import { failure, requiredString } from './http';

const WINDOW_SECONDS = 5 * 60;
const encoder = new TextEncoder();

interface SignedHeaders {
  machineID: string;
  timestamp: string;
  nonce: string;
  signature: string;
}

export async function signedMachineRequest(
  req: Request, sw: any, bodyText: string, registrationKey?: string,
): Promise<{ user: any; machine: any | null; machineID: string }> {
  const user = await sw.auth.requireUser(req);
  const headers = readSignedHeaders(req);
  validateTimestamp(headers.timestamp);
  const found = await sw.db.from('machines', { where: { id: headers.machineID }, limit: 1 });
  const machine = found.data[0] ?? null;
  const publicKey = machine?.machine_public_key || registrationKey;
  if (!publicKey) throw requestError(404, 'MACHINE_NOT_FOUND', 'machine is not registered');
  await verifySignature(req, bodyText, headers, publicKey);
  await consumeNonce(sw, headers.machineID, headers.nonce);
  return { user, machine, machineID: headers.machineID };
}

function readSignedHeaders(req: Request): SignedHeaders {
  const machineID = requiredString(req.headers.get('X-Sessions-Machine-ID'), 'X-Sessions-Machine-ID');
  const timestamp = requiredString(req.headers.get('X-Sessions-Timestamp'), 'X-Sessions-Timestamp');
  const nonce = requiredString(req.headers.get('X-Sessions-Nonce'), 'X-Sessions-Nonce');
  const signature = requiredString(req.headers.get('X-Sessions-Signature'), 'X-Sessions-Signature');
  if (nonce.length > 128 || signature.length > 256) {
    throw requestError(400, 'INVALID_SIGNATURE', 'signed request headers are too large');
  }
  return { machineID, timestamp, nonce, signature };
}

function validateTimestamp(value: string): void {
  if (!/^\d{10,12}$/.test(value)) throw requestError(401, 'STALE_SIGNATURE', 'signed request timestamp is invalid');
  const seconds = Number(value);
  if (!Number.isSafeInteger(seconds) || Math.abs(Math.floor(Date.now() / 1000) - seconds) > WINDOW_SECONDS) {
    throw requestError(401, 'STALE_SIGNATURE', 'signed request is outside the five-minute window');
  }
}

async function verifySignature(
  req: Request, bodyText: string, headers: SignedHeaders, publicText: string,
): Promise<void> {
  try {
    const publicKey = await crypto.subtle.importKey(
      'raw', decodeBase64URL(publicText), { name: 'Ed25519' }, false, ['verify'],
    );
    const bodyHash = await crypto.subtle.digest('SHA-256', encoder.encode(bodyText));
    const canonical = headers.machineID + headers.timestamp + headers.nonce
      + req.method + new URL(req.url).pathname + hex(bodyHash);
    const valid = await crypto.subtle.verify(
      { name: 'Ed25519' }, publicKey, decodeBase64URL(headers.signature), encoder.encode(canonical),
    );
    if (!valid) throw requestError(401, 'INVALID_SIGNATURE', 'machine signature is invalid');
  } catch (error) {
    if ((error as { code?: string }).code) throw error;
    throw requestError(401, 'INVALID_SIGNATURE', 'machine signature is invalid');
  }
}

async function consumeNonce(sw: any, machineID: string, nonce: string): Promise<void> {
  const cutoff = new Date(Date.now() - WINDOW_SECONDS * 1000).toISOString();
  await sw.db.remove('machine_nonces', { where: { created_at: { lt: cutoff } } });
  const inserted = await sw.db.insert('machine_nonces', {
    machine_id: machineID, nonce, created_at: new Date().toISOString(),
  }, { onConflict: 'ignore' });
  if (inserted.changes !== 1) throw requestError(409, 'NONCE_REPLAYED', 'signed request nonce was already used');
}

function decodeBase64URL(value: string): ArrayBuffer {
  const normalized = value.replace(/-/g, '+').replace(/_/g, '/');
  const padded = normalized + '='.repeat((4 - normalized.length % 4) % 4);
  const binary = atob(padded);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0)).buffer as ArrayBuffer;
}

function hex(value: ArrayBuffer): string {
  return Array.from(new Uint8Array(value), (byte) => byte.toString(16).padStart(2, '0')).join('');
}

function requestError(status: number, code: string, message: string): Error {
  return Object.assign(new Error(message), { status, code });
}

export function machineFailure(error: unknown): Response {
  return failure(error);
}
