import { useEffect, useRef, useState, type ClipboardEvent, type DragEvent, type KeyboardEvent } from 'react';
import { uploadFile } from '../api/sessionsd';
import type { SessionTool } from '../types';
import { ComposerModelControl } from './ComposerModelControl';

interface Props {
  // Sender from useTerminal — writes through the live WS. xterm echoes
  // the bytes back into the buffer so the terminal stays the source of truth.
  send: (data: string) => void;
  // Status from useTerminal — disable when not open.
  connected: boolean;
  // Session id — needed for file uploads so the server knows which
  // session's uploads dir to use (so user types in the path of their
  // dropped file as a result of drag-drop).
  sessionId: string;
  // Fires AFTER bytes leave (immediately after submit). Used by the
  // parent to render an optimistic "pending" message in the Sessions
  // view so the user sees their message land instantly, instead of
  // waiting for Claude's TUI redraw + parser throttle (~500ms-1s).
  onSubmitted?: (text: string) => void;
  // Failed Remote sends restore their text here so the user's draft is
  // recoverable without copy/pasting from the red bubble. The version
  // changes per failed attempt.
  recoverDraft?: { id: string; text: string; version: number } | null;
  provider?: SessionTool;
  model?: string;
  effort?: string;
  modelControlSupported?: boolean;
  providerWorking?: boolean;
  onConfigureModel?: (model: string, effort: string) => Promise<void>;
}

// Quote a path for safe shell-style insertion. Single quotes wrap the
// whole thing; embedded single quotes are escaped via the
// '"'"' standard trick. Claude reads the path from text, so a properly
// quoted path lets messages with spaces / quotes work too.
function quotePath(p: string): string {
  return "'" + p.replace(/'/g, "'\"'\"'") + "'";
}

// Bottom composer for the Sessions view. xterm itself accepts input fine
// when focused, but in Sessions mode the user can't see the cursor — they
// need an obvious "type here" target. Keystrokes go through the same WS
// as xterm's onData; the PTY echoes them and the parser sees them on the
// next snapshot. No "live-type diff" machinery from sessions-tmux: with a
// real PTY, the echoed text just appears.
export function InputBar({
  send,
  connected,
  sessionId,
  onSubmitted,
  recoverDraft,
  provider = 'claude-code',
  model,
  effort,
  modelControlSupported = false,
  providerWorking = false,
  onConfigureModel
}: Props): JSX.Element {
  const [text, setText] = useState('');
  // 'idle' | 'sent' — sent briefly turns the Send button green so the
  // user can see the bytes left this client. The button text stays
  // "Send" the entire time; the green flash IS the feedback. (No ✓
  // overlay or "Sent" label — those read as "task completed" or
  // "message acknowledged by the recipient", which is a different
  // claim than "the bytes left your browser".)
  const [feedback, setFeedback] = useState<'idle' | 'sent'>('idle');
  const [dragOver, setDragOver] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const taRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const handledRecoveryKeysRef = useRef<Set<string>>(new Set());
  const restoredDraftRef = useRef<{ key: string; text: string } | null>(null);

  // Auto-dismiss the upload error after 8s — the user may have moved on
  // and it's annoying to have a persistent red stripe that they can't
  // dismiss. Refreshes the timer each time a new error is set.
  useEffect(() => {
    if (!uploadError) return;
    const id = window.setTimeout(() => setUploadError(null), 8000);
    return () => window.clearTimeout(id);
  }, [uploadError]);

  useEffect(() => {
    if (!recoverDraft) {
      const restored = restoredDraftRef.current;
      if (restored && text === restored.text) {
        setText('');
      }
      restoredDraftRef.current = null;
      return;
    }

    const key = `${recoverDraft.id}:${recoverDraft.version}`;
    if (handledRecoveryKeysRef.current.has(key)) return;
    if (text.trim().length > 0) return;

    handledRecoveryKeysRef.current.add(key);
    restoredDraftRef.current = { key, text: recoverDraft.text };
    setText(recoverDraft.text);
  }, [recoverDraft, text]);

  const submit = (): void => {
    if (!connected) return;
    setUploadError(null); // clear any lingering upload error on submit
    if (text) {
      // Two-step submit:
      //   1. Send the text wrapped in bracketed-paste markers so
      //      Claude Code's TUI (Ink + bracketed-paste mode) knows
      //      this is paste, not a fast keystroke storm. The end
      //      marker \x1b[201~ tells it where the paste finishes.
      //   2. After a tiny delay, send \r as its OWN WS message.
      //      That arrives at the runner as a separate pty.write,
      //      which Ink's input loop reads as a fresh keystroke.
      //      Without the delay, Ink processes the paste-end marker
      //      AND the trailing \r in the same read() call — the \r
      //      gets buffered and doesn't fire submit until the next
      //      keystroke arrives. (That's why a second Enter "fixed"
      //      it: the second Enter's \r came in cleanly on its own.)
      send('\x1b[200~' + text + '\x1b[201~');
      window.setTimeout(() => send('\r'), 30);
    } else {
      // Empty buffer — just an Enter, e.g. to accept a y/n prompt.
      send('\r');
    }
    if (text && onSubmitted) onSubmitted(text);
    setText('');
    restoredDraftRef.current = null;
    setFeedback('sent');
    window.setTimeout(() => setFeedback('idle'), 500);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>): void => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submit();
      return;
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      if (text) {
        setText('');
      } else {
        send('\x1b');
      }
      return;
    }
  };

  // Shared upload + insert-path-at-cursor flow. Used by drag-drop AND
  // paste (Cmd-V of a screenshot or copied image). We never auto-submit
  // — the user adds a caption then hits Send.
  const uploadAndInsert = async (files: File[]): Promise<void> => {
    if (files.length === 0) return;
    setUploading(true);
    setUploadError(null);
    const paths: string[] = [];
    try {
      for (const f of files) {
        const { path } = await uploadFile(sessionId, f);
        paths.push(quotePath(path));
      }
      const ta = taRef.current;
      if (ta) {
        const start = ta.selectionStart ?? text.length;
        const end = ta.selectionEnd ?? text.length;
        const before = text.slice(0, start);
        const after = text.slice(end);
        const insert = (before && !before.endsWith(' ') ? ' ' : '') + paths.join(' ') + ' ';
        setText(before + insert + after);
        requestAnimationFrame(() => {
          const pos = (before + insert).length;
          ta.focus();
          ta.setSelectionRange(pos, pos);
        });
      } else {
        setText((t) => (t && !t.endsWith(' ') ? t + ' ' : t) + paths.join(' ') + ' ');
      }
    } catch (err) {
      setUploadError((err as Error).message);
    } finally {
      setUploading(false);
    }
  };

  // Drag-drop file handling. Upload each dropped file to the sessionsd
  // host's uploads dir, then paste the (single-quoted) absolute path
  // into the textarea. We DON'T auto-submit — the user typically wants
  // to add a caption ("describe this image", "what's wrong here", etc.)
  // before sending. Multiple files dropped in one event are appended
  // space-separated.
  const onDragOver = (e: DragEvent<HTMLDivElement>): void => {
    if (e.dataTransfer?.types?.includes('Files')) {
      e.preventDefault();
      setDragOver(true);
    }
  };
  const onDragLeave = (): void => setDragOver(false);
  const onDrop = async (e: DragEvent<HTMLDivElement>): Promise<void> => {
    e.preventDefault();
    setDragOver(false);
    const files = Array.from(e.dataTransfer?.files ?? []);
    await uploadAndInsert(files);
  };

  // Paste handling. Cmd-V (screenshot from system clipboard, or any
  // image copied from another app) lands here as a clipboard paste
  // event with image/* DataTransferItems. We pull the Blobs, give
  // each a sensible filename derived from MIME, then run them through
  // the same upload-and-insert flow as drag-drop. Plain text pastes
  // (the much more common case) fall through to the textarea's default
  // handling — we only intercept when there's an image present.
  const onPaste = (e: ClipboardEvent<HTMLTextAreaElement>): void => {
    const items = Array.from(e.clipboardData?.items ?? []);
    const imageItems = items.filter((it) => it.kind === 'file' && it.type.startsWith('image/'));
    if (imageItems.length === 0) return;
    e.preventDefault();
    const files: File[] = [];
    for (const it of imageItems) {
      const blob = it.getAsFile();
      if (!blob) continue;
      // Browsers usually give pasted images a generic "image.png"
      // filename; normalize so the uploads dir lists are readable.
      const ext = (blob.type.split('/')[1] ?? 'png').replace(/\W/g, '').slice(0, 8);
      const name = blob.name && blob.name !== 'image.png' ? blob.name : `paste-${Date.now()}.${ext}`;
      files.push(new File([blob], name, { type: blob.type }));
    }
    void uploadAndInsert(files);
  };

  return (
    <div
      className={`input-bar${dragOver ? ' is-drag-over' : ''}${uploading ? ' is-uploading' : ''}`}
      onDragOver={onDragOver}
      onDragLeave={onDragLeave}
      onDrop={onDrop}
    >
      {dragOver ? (
        <div className="input-bar-drop-overlay">Drop file to attach…</div>
      ) : null}
      {uploading ? (
        <div className="input-bar-upload-state">Uploading…</div>
      ) : null}
      {uploadError ? (
        <div className="input-bar-upload-state is-error">
          <span>Upload failed: {uploadError}</span>
          {/* Dismiss button — the error auto-clears after 8s but users
              shouldn't have to wait; also clears on next successful
              submit or when the textarea is emptied. */}
          <button
            type="button"
            className="input-bar-upload-dismiss"
            aria-label="Dismiss upload error"
            onClick={() => setUploadError(null)}
          >×</button>
        </div>
      ) : null}
      <div className="input-composer">
        <input
          ref={fileInputRef}
          className="input-file-picker"
          type="file"
          multiple
          onChange={(event) => {
            const files = Array.from(event.currentTarget.files ?? []);
            if (files.length > 0) void uploadAndInsert(files);
            event.currentTarget.value = '';
          }}
        />
        <textarea
          ref={taRef}
          className="input-textarea"
          value={text}
          onChange={(e) => {
            setText(e.target.value);
            // Clear a stale upload error when the user wipes the draft —
            // the attached file path is gone so the error is no longer
            // actionable.
            if (!e.target.value) setUploadError(null);
          }}
          onKeyDown={onKeyDown}
          onPaste={onPaste}
          placeholder={connected
            ? `Message ${provider === 'codex' ? 'Codex' : 'Claude'} — Enter sends, Shift+Enter for newline`
            : 'Disconnected'}
          disabled={!connected}
          rows={Math.min(6, Math.max(1, text.split('\n').length))}
          autoCapitalize="sentences"
          autoCorrect="on"
          spellCheck
        />
        <div className="input-composer-footer">
          <button
            type="button"
            className="input-attach"
            disabled={!connected || uploading}
            onClick={() => fileInputRef.current?.click()}
            title="Attach files"
          >
            <span aria-hidden>＋</span><span>Attach</span>
          </button>
          <span className="input-composer-spacer" />
          {provider !== 'terminal' && onConfigureModel ? (
            <ComposerModelControl
              sessionId={sessionId}
              provider={provider}
              model={model}
              effort={effort}
              supported={modelControlSupported}
              working={providerWorking}
              onChange={onConfigureModel}
            />
          ) : null}
          <button
            type="button"
            className={`btn btn-primary input-send${feedback === 'sent' ? ' is-sent' : ''}`}
            onClick={submit}
            disabled={!connected}
            aria-label="Send"
            title="Send (Enter)"
          >
            <span aria-hidden>↑</span>
          </button>
        </div>
      </div>
    </div>
  );
}
