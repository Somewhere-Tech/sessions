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
  if (session.tool === 'claude-code') {
    return session.args.some((arg) => arg === '--remote-control' || arg.startsWith('--remote-control='))
      ? 'Claude Remote Control — Conversation + Terminal'
      : 'Claude interactive — Conversation + Terminal';
  }
  return 'Terminal compatibility — Shell in a PTY';
}

export function sessionModeShort(session: SessionInfo): 'Rich' | 'Terminal' | 'Remote Control' | 'Interactive' {
  if (session.tool === 'claude-code' && session.kind !== 'claude-structured') {
    return session.args.some((arg) => arg === '--remote-control' || arg.startsWith('--remote-control='))
      ? 'Remote Control'
      : 'Interactive';
  }
  return sessionMode(session) === 'rich' ? 'Rich' : 'Terminal';
}

export function sessionModeGlyph(session: SessionInfo): '◆' | '▮' {
  return sessionMode(session) === 'rich' ? '◆' : '▮';
}
