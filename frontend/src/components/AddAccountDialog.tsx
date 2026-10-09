import { useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { MachineAccounts } from '../lib/accountRollup';
import { ProviderMark } from './ProviderBadge';

interface Props {
  busy: boolean;
  computers: MachineAccounts[];
  initialComputer: string;
  error: string | null;
  onAdd: (computer: string, tool: 'claude' | 'codex', label: string) => void;
  onCancel: () => void;
}

/** Keep keyboard navigation in the picker and return to its + button on close. */
function usePickerFocus(ref: React.RefObject<HTMLElement>, busy: boolean, onCancel: () => void): void {
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    ref.current?.querySelector<HTMLButtonElement>('button')?.focus();
    return () => { if (previous?.isConnected) previous.focus(); };
  }, [ref]);
  useEffect(() => {
    const keepFocus = (): void => {
      if (!ref.current || ref.current.contains(document.activeElement)) return;
      (ref.current.querySelector<HTMLElement>('button:not(:disabled), a[href]') ?? ref.current).focus();
    };
    keepFocus();
    const onKey = (event: KeyboardEvent): void => {
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); if (!busy) onCancel(); }
      if (event.key !== 'Tab') return;
      const buttons = [...(ref.current?.querySelectorAll<HTMLElement>('button:not(:disabled), a[href]') ?? [])];
      const first = buttons[0], last = buttons[buttons.length - 1];
      if (!first) { event.preventDefault(); ref.current?.focus(); }
      else if (!ref.current?.contains(document.activeElement) || document.activeElement === ref.current) {
        event.preventDefault(); (event.shiftKey ? last : first)?.focus();
      }
      else if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
    };
    document.addEventListener('keydown', onKey);
    document.addEventListener('focusin', keepFocus);
    return () => { document.removeEventListener('keydown', onKey); document.removeEventListener('focusin', keepFocus); };
  }, [ref, busy, onCancel]);
}

export function AddAccountDialog({ busy, computers, initialComputer, error, onAdd, onCancel }: Props): JSX.Element {
  const [tool, setTool] = useState<'claude' | 'codex' | null>(null);
  const [computer, setComputer] = useState(() => computers.some((machine) => machine.serverId === initialComputer)
    ? initialComputer : computers[0]?.serverId ?? '');
  const titleId = useId();
  const ref = useRef<HTMLElement>(null);
  usePickerFocus(ref, busy, onCancel);
  const machineName = computers.find((machine) => machine.serverId === computer)?.machineName;
  return createPortal(
    <div className="dialog-backdrop account-picker-backdrop" onClick={(event) => { if (event.target === event.currentTarget && !busy) onCancel(); }}>
      <section ref={ref} className="account-picker" role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1}>
        <header className="account-picker-head">
          <div><h2 id={titleId}>Add account</h2><p>Your subscriptions, together.</p></div>
          <button type="button" className="account-picker-close" aria-label="Close account picker" disabled={busy} onClick={onCancel}>×</button>
        </header>
        <div className="account-picker-body">
          {computers.length > 1 ? <div className="account-picker-computers" role="group" aria-label="Computer">
            {computers.map((machine) => <button type="button" key={machine.serverId} disabled={busy}
              aria-pressed={computer === machine.serverId} onClick={() => setComputer(machine.serverId)}>{machine.machineName}</button>)}
          </div> : <p className="account-picker-location">Connecting on {machineName ?? 'this computer'}</p>}
          <div className="account-picker-providers" role="group" aria-label="Provider">
            <ProviderChoice tool="claude" selected={tool === 'claude'} busy={busy} onSelect={setTool} />
            <ProviderChoice tool="codex" selected={tool === 'codex'} busy={busy} onSelect={setTool} />
          </div>
          {tool === 'codex' ? <DeviceSignInHelp /> : <p className="account-picker-note">Sign in with the provider. No API key needed. You can rename the account afterwards.</p>}
          {error ? <p className="settings-message" role="alert">{error}</p> : null}
        </div>
        <footer className="account-picker-footer">
          <span>Your other accounts stay signed in.</span>
          <button type="button" className="btn btn-primary" disabled={busy || !tool || !machineName}
            onClick={() => { if (tool) onAdd(computer, tool, ''); }}>{busy ? 'Preparing…' : tool ? `Sign in to ${tool === 'codex' ? 'ChatGPT' : 'Claude'}` : 'Choose a provider'}</button>
        </footer>
      </section>
    </div>, document.querySelector('.operations-shell') ?? document.body
  );
}

function ProviderChoice({ tool, selected, busy, onSelect }: {
  tool: 'claude' | 'codex'; selected: boolean; busy: boolean; onSelect: (tool: 'claude' | 'codex') => void;
}): JSX.Element {
  const name = tool === 'codex' ? 'ChatGPT' : 'Claude';
  return <button type="button" className="account-provider-choice" aria-label={name} aria-pressed={selected}
    disabled={busy} onClick={() => onSelect(tool)}>
    <ProviderMark provider={tool} size={36} />
    <span><strong>{name}</strong><small>{tool === 'codex' ? 'Use your plan with Codex' : 'Use your Claude Code subscription'}</small></span>
    <span className="account-provider-selection" aria-hidden="true">{selected ? '✓' : '+'}</span>
  </button>;
}

export function DeviceSignInHelp(): JSX.Element {
  return <aside className="account-device-help">
    <strong>One-time ChatGPT setting</strong>
    <p>This sign-in uses a device code. If OpenAI asks, enable device-code sign-in in ChatGPT Settings → Security, then retry. Managed workspaces may need an administrator.</p>
    <a href="https://chatgpt.com/#settings/Security" target="_blank" rel="noreferrer">Open ChatGPT security settings ↗</a>
  </aside>;
}
