// Three-step interface size. Stored locally per device; each step maps
// to a class on .app-shell. The operations UI scales as one surface so
// navigation, settings, conversations, and controls keep their visual
// proportions instead of changing only transcript copy.

export type TextSize = 'S' | 'M' | 'L';

const KEY = 'sessions:text-size';

export function readTextSize(): TextSize {
  try {
    const v = window.localStorage.getItem(KEY);
    if (v === 'S' || v === 'M' || v === 'L') return v;
  } catch { /* ignore */ }
  return 'M';
}

export function writeTextSize(size: TextSize): void {
  try {
    window.localStorage.setItem(KEY, size);
  } catch { /* ignore */ }
}

export function nextSize(s: TextSize): TextSize {
  return s === 'S' ? 'M' : s === 'M' ? 'L' : 'S';
}

export function sizeLabel(s: TextSize): string {
  return s === 'S' ? 'Compact' : s === 'M' ? 'Comfortable' : 'Large';
}
