import type { SessionInfo } from '../types';

export type SessionMode = 'rich' | 'terminal';

export function sessionMode(session: SessionInfo): SessionMode {
  return session.kind === 'codex-app-server' || session.kind === 'claude-structured'
    ? 'rich'
    : 'terminal';
}

export function sessionModeName(session: SessionInfo): string {
  if (session.kind === 'codex-app-server') return 'Rich — Codex app-server';
  if (session.kind === 'claude-structured') return 'Rich — Claude structured';
  if (session.tool === 'codex') return 'Terminal compatibility — Codex in a PTY';
  if (session.tool === 'claude-code') return 'Terminal compatibility — Claude in a PTY';
  return 'Terminal compatibility — Shell in a PTY';
}

export function sessionModeShort(session: SessionInfo): 'Rich' | 'Terminal' {
  return sessionMode(session) === 'rich' ? 'Rich' : 'Terminal';
}

export function sessionModeGlyph(session: SessionInfo): '◆' | '▮' {
  return sessionMode(session) === 'rich' ? '◆' : '▮';
}
