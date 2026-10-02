export type NewSessionTool = 'claude-code' | 'codex' | 'shell';

export interface NewSessionDefaults {
  tool: NewSessionTool;
  skipPerms: boolean;
  cwd: string;
  cols: number;
  rows: number;
  tags: Record<string, string>;
}

export const NEW_SESSION_DEFAULTS_KEY = 'sessions:new-session-defaults';

export const DEFAULT_NEW_SESSION_DEFAULTS: NewSessionDefaults = {
  tool: 'claude-code',
  skipPerms: true,
  cwd: '',
  cols: 300,
  rows: 50,
  tags: {}
};

export const NEW_SESSION_DIMENSIONS = {
  minCols: 40,
  maxCols: 500,
  minRows: 10,
  maxRows: 200
} as const;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function isNewSessionTool(value: unknown): value is NewSessionTool {
  return value === 'claude-code' || value === 'codex' || value === 'shell';
}

function normalizeDimension(
  value: unknown,
  fallback: number,
  min: number,
  max: number
): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) return fallback;
  return Math.max(min, Math.min(max, Math.trunc(value)));
}

const TAG_KEY = /^[a-z0-9][a-z0-9._-]*$/;

function normalizeTags(value: unknown): Record<string, string> {
  if (!isRecord(value)) return {};
  const tags: Record<string, string> = {};
  for (const [rawKey, rawValue] of Object.entries(value)) {
    if (Object.keys(tags).length >= 32) break;
    const key = rawKey.trim().toLowerCase();
    const tagValue = typeof rawValue === 'string' ? rawValue.trim() : '';
    if (!TAG_KEY.test(key) || key.length > 64 || !tagValue || tagValue.length > 256) continue;
    tags[key] = tagValue;
  }
  return tags;
}

export function normalizeNewSessionDefaults(value: unknown): NewSessionDefaults {
  const record = isRecord(value) ? value : {};
  return {
    tool: isNewSessionTool(record.tool) ? record.tool : DEFAULT_NEW_SESSION_DEFAULTS.tool,
    skipPerms: typeof record.skipPerms === 'boolean'
      ? record.skipPerms
      : DEFAULT_NEW_SESSION_DEFAULTS.skipPerms,
    cwd: typeof record.cwd === 'string' ? record.cwd : DEFAULT_NEW_SESSION_DEFAULTS.cwd,
    cols: normalizeDimension(
      record.cols,
      DEFAULT_NEW_SESSION_DEFAULTS.cols,
      NEW_SESSION_DIMENSIONS.minCols,
      NEW_SESSION_DIMENSIONS.maxCols
    ),
    rows: normalizeDimension(
      record.rows,
      DEFAULT_NEW_SESSION_DEFAULTS.rows,
      NEW_SESSION_DIMENSIONS.minRows,
      NEW_SESSION_DIMENSIONS.maxRows
    ),
    tags: normalizeTags(record.tags)
  };
}

export function readNewSessionDefaults(): NewSessionDefaults {
  try {
    const raw = window.localStorage.getItem(NEW_SESSION_DEFAULTS_KEY);
    if (!raw) return DEFAULT_NEW_SESSION_DEFAULTS;
    const parsed: unknown = JSON.parse(raw);
    return normalizeNewSessionDefaults(parsed);
  } catch {
    return DEFAULT_NEW_SESSION_DEFAULTS;
  }
}

export function writeNewSessionDefaults(defaults: NewSessionDefaults): void {
  try {
    window.localStorage.setItem(
      NEW_SESSION_DEFAULTS_KEY,
      JSON.stringify(normalizeNewSessionDefaults(defaults))
    );
  } catch { /* quota / private mode - non-fatal */ }
}
