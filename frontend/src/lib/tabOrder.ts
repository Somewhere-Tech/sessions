// User-defined tab order. Sessions come back from sessionsd in arbitrary
// order (creation time, usually), but the user wants to organize them
// — drag this one ahead, group related projects together, etc.
//
// Stored as a flat array of session IDs. Any session not present in
// the order falls to the end (so newly-spawned sessions land at the
// right of the tab strip, ready to be dragged into place).

const STORAGE_KEY = 'sessions:tab-order:v1';

export function readTabOrder(): string[] {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((x) => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

export function writeTabOrder(order: string[]): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(order));
  } catch {
    // quota / private mode — non-fatal
  }
}

// Sort `items` according to `order`. Items present in `order` appear in
// that order; items not present (new sessions) keep their original
// relative position at the end.
export function applyOrder<T extends { id: string }>(items: T[], order: string[]): T[] {
  const indexOf = new Map<string, number>();
  order.forEach((id, i) => indexOf.set(id, i));
  // Stable partition: ordered first (sorted by their order index),
  // unknown after (original order preserved).
  const ordered: T[] = [];
  const unknown: T[] = [];
  for (const item of items) {
    if (indexOf.has(item.id)) ordered.push(item);
    else unknown.push(item);
  }
  ordered.sort((a, b) => (indexOf.get(a.id) ?? 0) - (indexOf.get(b.id) ?? 0));
  return [...ordered, ...unknown];
}

// Move the entry with `fromId` so it lands immediately BEFORE `toId`
// in the resulting order. If either id is missing from the current
// `order`, it's inserted from `currentIds` (the full known list) so
// nothing gets lost mid-reorder.
export function moveBefore(order: string[], currentIds: string[], fromId: string, toId: string): string[] {
  if (fromId === toId) return order;
  // Build an order that contains every known id (filling in any
  // missing ones at the end). Operating on this ensures we never
  // drop a session by reordering.
  const full = [...order];
  for (const id of currentIds) if (!full.includes(id)) full.push(id);

  const fromIdx = full.indexOf(fromId);
  if (fromIdx === -1) return order;
  full.splice(fromIdx, 1);
  const toIdx = full.indexOf(toId);
  if (toIdx === -1) {
    full.push(fromId);
  } else {
    full.splice(toIdx, 0, fromId);
  }
  return full;
}
