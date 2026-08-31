export type TerminalBufferType = 'normal' | 'alternate';

export interface TerminalViewportPosition {
  type: TerminalBufferType;
  viewportY: number;
  baseY: number;
}

interface BufferIntent {
  following: boolean;
  anchorY: number;
}

// xterm's onScroll event describes movement, not intent. Output, fit, replay,
// and buffer swaps can all fire it. This coordinator changes follow intent
// only when the caller has observed a real user scroll gesture; every terminal
// mutation merely snapshots and restores the already-owned intent.
export class TerminalViewportCoordinator {
  private readonly buffers: Record<TerminalBufferType, BufferIntent> = {
    normal: { following: true, anchorY: 0 },
    alternate: { following: true, anchorY: 0 }
  };

  userScrolled(position: TerminalViewportPosition): void {
    const state = this.buffers[position.type];
    state.following = position.viewportY >= position.baseY - 2;
    state.anchorY = position.viewportY;
  }

  followLatest(position: TerminalViewportPosition): void {
    const state = this.buffers[position.type];
    state.following = true;
    state.anchorY = position.baseY;
  }

  beforeMutation(position: TerminalViewportPosition): void {
    const state = this.buffers[position.type];
    if (!state.following) state.anchorY = position.viewportY;
  }

  restoreAfterMutation(
    position: TerminalViewportPosition,
    scrollLines: (amount: number) => void,
    scrollToBottom: () => void
  ): void {
    const state = this.buffers[position.type];
    if (state.following) {
      scrollToBottom();
      state.anchorY = position.baseY;
      return;
    }
    const target = Math.max(0, Math.min(state.anchorY, position.baseY));
    const delta = target - position.viewportY;
    if (delta !== 0) scrollLines(delta);
    state.anchorY = target;
  }

  isFollowing(position: TerminalViewportPosition): boolean {
    return this.buffers[position.type].following;
  }
}

export function isTerminalScrollKey(event: Pick<KeyboardEvent, 'key' | 'shiftKey'>): boolean {
  if (event.key === 'PageUp' || event.key === 'PageDown') return true;
  if ((event.key === 'Home' || event.key === 'End') && event.shiftKey) return true;
  return event.shiftKey && (event.key === 'ArrowUp' || event.key === 'ArrowDown');
}
