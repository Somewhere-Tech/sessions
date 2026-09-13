const DRAFT_PREFIX = 'sessions:text-draft:v1:';
const MAX_DRAFTS = 64;
const MAX_DRAFT_BYTES = 64 * 1024;
const MAX_TOTAL_BYTES = 512 * 1024;

interface StoredDraft {
  text: string;
  updatedAt: number;
}

export interface DraftSaveResult {
  saved: boolean;
  warning?: string;
}

export interface DraftReadResult {
  text: string;
  warning?: string;
}

export function draftStorageKey(machineId: string, sessionId: string): string {
  return `${DRAFT_PREFIX}${encodeURIComponent(machineId)}:${encodeURIComponent(sessionId)}`;
}

function encodedBytes(value: string): number {
  return new TextEncoder().encode(value).byteLength;
}

function draftEntries(storage: Storage, except: string): Array<[string, string]> {
  const entries: Array<[string, string]> = [];
  for (let index = 0; index < storage.length; index += 1) {
    const key = storage.key(index);
    if (!key?.startsWith(DRAFT_PREFIX) || key === except) continue;
    const value = storage.getItem(key);
    if (value !== null) entries.push([key, value]);
  }
  return entries;
}

export function readDraft(key: string): DraftReadResult {
  try {
    const storage = window.localStorage;
    const raw = storage.getItem(key);
    if (!raw) return { text: '' };
    const parsed = JSON.parse(raw) as Partial<StoredDraft>;
    return { text: typeof parsed.text === 'string' ? parsed.text : '' };
  } catch {
    return { text: '', warning: 'Saved drafts could not be read on this device. Keep this window open or copy new text somewhere safe.' };
  }
}

export function saveDraft(key: string, text: string): DraftSaveResult {
  try {
    const storage = window.localStorage;
    if (!text) {
      storage.removeItem(key);
      return { saved: true };
    }
    const serialized = JSON.stringify({ text, updatedAt: Date.now() } satisfies StoredDraft);
    if (encodedBytes(serialized) > MAX_DRAFT_BYTES) {
      return { saved: false, warning: 'The latest edits make this draft too large to save locally. Keep this window open or copy it somewhere safe.' };
    }
    const others = draftEntries(storage, key);
    if (others.length >= MAX_DRAFTS || encodedBytes(serialized) + others.reduce((sum, [, value]) => sum + encodedBytes(value), 0) > MAX_TOTAL_BYTES) {
      return { saved: false, warning: 'The latest edits are not saved because draft storage is full. Clear an older draft or copy this text somewhere safe before closing.' };
    }
    storage.setItem(key, serialized);
    return { saved: true };
  } catch {
    return { saved: false, warning: 'The latest edits could not be saved on this device. Keep this window open or copy them somewhere safe.' };
  }
}
