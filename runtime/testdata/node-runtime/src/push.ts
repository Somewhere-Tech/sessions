import fs from 'node:fs';
import nodePath from 'node:path';
import webpush from 'web-push';
import type { PushSubscription } from 'web-push';
import { PRETTYD_STATE_DIR } from './config.js';

const VAPID_PATH = nodePath.join(PRETTYD_STATE_DIR, 'vapid.json');
const SUBSCRIPTIONS_PATH = nodePath.join(PRETTYD_STATE_DIR, 'push-subscriptions.json');
const VAPID_SUBJECT = 'mailto:pretty-pty@localhost';

interface VapidKeys {
  publicKey: string;
  privateKey: string;
}

export type PushSubscriptionRecord = PushSubscription;

export interface PushPayload {
  title: string;
  body?: string;
  data?: Record<string, unknown>;
}

function ensureStateDir(): void {
  fs.mkdirSync(PRETTYD_STATE_DIR, { recursive: true, mode: 0o700 });
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function isVapidKeys(value: unknown): value is VapidKeys {
  return isObject(value)
    && typeof value.publicKey === 'string'
    && value.publicKey.length > 0
    && typeof value.privateKey === 'string'
    && value.privateKey.length > 0;
}

function isPushSubscription(value: unknown): value is PushSubscriptionRecord {
  if (!isObject(value)) return false;
  const keys = value.keys;
  return typeof value.endpoint === 'string'
    && value.endpoint.length > 0
    && (value.expirationTime === undefined || value.expirationTime === null || typeof value.expirationTime === 'number')
    && isObject(keys)
    && typeof keys.p256dh === 'string'
    && keys.p256dh.length > 0
    && typeof keys.auth === 'string'
    && keys.auth.length > 0;
}

function readVapidKeys(): VapidKeys | null {
  try {
    const parsed = JSON.parse(fs.readFileSync(VAPID_PATH, 'utf8')) as unknown;
    return isVapidKeys(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

function writeVapidKeys(keys: VapidKeys): void {
  ensureStateDir();
  fs.writeFileSync(VAPID_PATH, JSON.stringify(keys, null, 2), { mode: 0o600 });
}

function getVapidKeys(): VapidKeys {
  const existing = readVapidKeys();
  if (existing) return existing;
  const generated = webpush.generateVAPIDKeys();
  writeVapidKeys(generated);
  return generated;
}

function loadSubscriptions(): PushSubscriptionRecord[] {
  try {
    const parsed = JSON.parse(fs.readFileSync(SUBSCRIPTIONS_PATH, 'utf8')) as unknown;
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(isPushSubscription);
  } catch {
    return [];
  }
}

function saveSubscriptions(subscriptions: PushSubscriptionRecord[]): void {
  ensureStateDir();
  fs.writeFileSync(SUBSCRIPTIONS_PATH, JSON.stringify(subscriptions, null, 2), { mode: 0o600 });
}

function getPushStatusCode(err: unknown): number | null {
  if (err instanceof webpush.WebPushError) return err.statusCode;
  if (!isObject(err)) return null;
  const statusCode = err.statusCode;
  return typeof statusCode === 'number' ? statusCode : null;
}

export function getVapidPublicKey(): string {
  return getVapidKeys().publicKey;
}

export function addSubscription(subscription: unknown): void {
  if (!isPushSubscription(subscription)) {
    throw new Error('invalid push subscription');
  }
  const subscriptions = loadSubscriptions();
  const withoutExisting = subscriptions.filter((s) => s.endpoint !== subscription.endpoint);
  saveSubscriptions([...withoutExisting, subscription]);
}

export function removeSubscription(endpoint: string): void {
  const subscriptions = loadSubscriptions();
  saveSubscriptions(subscriptions.filter((s) => s.endpoint !== endpoint));
}

export async function sendPush(payload: PushPayload): Promise<void> {
  let keys: VapidKeys;
  let subscriptions: PushSubscriptionRecord[];
  try {
    keys = getVapidKeys();
    subscriptions = loadSubscriptions();
  } catch {
    return;
  }

  for (const subscription of subscriptions) {
    try {
      await webpush.sendNotification(subscription, JSON.stringify(payload), {
        vapidDetails: {
          subject: VAPID_SUBJECT,
          publicKey: keys.publicKey,
          privateKey: keys.privateKey
        },
        TTL: 60 * 60,
        urgency: 'normal'
      });
    } catch (err) {
      const statusCode = getPushStatusCode(err);
      if (statusCode === 404 || statusCode === 410) {
        try { removeSubscription(subscription.endpoint); } catch { /* best effort */ }
      }
    }
  }
}
