import { useEffect, useRef, useState, type ClipboardEvent, type DragEvent, type KeyboardEvent } from 'react';
import { uploadFile } from '../api/sessionsd';
import type { SessionTool } from '../types';
import { ComposerModelControl } from './ComposerModelControl';

interface Props {
  // Acknowledged sender from useTerminal. Failed sends leave the draft visible
  // for retry; active Codex turns use the provider's native steering queue.
  send: (data: string) => Promise<void>;
  submitMessage: (data: string) => Promise<void>;
  // Display-stream status. This may reconnect independently of the durable
  // session and must not, by itself, disable an acknowledged message send.
  connected: boolean;
  // Authoritative daemon lifecycle state. A live, reachable session remains
  // sendable even while its display stream reconnects; the acknowledged send
  // either succeeds now or leaves the draft in place with the exact error.
  sendAvailable?: boolean;
  // Session id — needed for file uploads so the server knows which
  // session's uploads dir to use (so user types in the path of their
  // dropped file as a result of drag-drop).
  sessionId: string;
  // Fires AFTER bytes leave (immediately after submit). Used by the
  // parent to render an optimistic "pending" message in the Sessions
  // view so the user sees their message land instantly, instead of
  // waiting for Claude's TUI redraw + parser throttle (~500ms-1s).
  onSubmitted?: (text: string, queued: boolean) => void;
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
  // Rich provider sessions have no interactive TUI. Claude slash commands
  // must never be pasted into their structured composer as ordinary chat.
  richSession?: boolean;
  onRename?: (name: string) => Promise<void>;
  onContinueInTerminal?: (enableRemoteControl: boolean) => Promise<void>;
}

interface ComposerNotice {
  tone: 'info' | 'error';
  title: string;
  detail: string;
  canContinueInTerminal?: boolean;
  enableRemoteControl?: boolean;
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
// next snapshot without a separate live-typing diff.
export function InputBar({
  send,
  submitMessage,
  connected,
  sendAvailable = connected,
  sessionId,
  onSubmitted,
  recoverDraft,
  provider = 'claude-code',
  model,
  effort,
  modelControlSupported = false,
  providerWorking = false,
  onConfigureModel,
  richSession = false,
  onRename,
  onContinueInTerminal
}: Props): JSX.Element {
  const [text, setText] = useState('');
  // 'idle' | 'sent' — sent briefly turns the Send button green so the
  // user can see the bytes left this client. The button text stays
  // "Send" the entire time; the green flash IS the feedback. (No ✓
  // overlay or "Sent" label — those read as "task completed" or
  // "message acknowledged by the recipient", which is a different
  // claim than "the bytes left your browser".)
  const [feedback, setFeedback] = useState<'idle' | 'sent'>('idle');
  const [submitting, setSubmitting] = useState(false);
  const [dragOver, setDragOver] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [composerNotice, setComposerNotice] = useState<ComposerNotice | null>(null);
  const [continuingInTerminal, setContinuingInTerminal] = useState(false);
  // React state does not update synchronously. Two Enter/click events in the
  // same browser turn both saw submitting=false and sent the same message;
  // the daemon accepted the first and the duplicate failed. This ref is the
  // synchronous, per-composer exactly-once gate while state remains the UI.
  const submitInFlightRef = useRef(false);
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

  const submit = async (): Promise<void> => {
    if (!sendAvailable || submitInFlightRef.current) return;
    setUploadError(null); // clear any lingering upload error on submit
    setComposerNotice(null);
    const trimmed = text.trim();

    if (richSession && /^\/rename(?:\s|$)/i.test(trimmed)) {
      const name = trimmed.replace(/^\/rename(?:\s+|$)/i, '').trim();
      if (!name) {
        setComposerNotice({
          tone: 'error',
          title: 'Add a name after /rename',
          detail: 'For example: /rename Database migration'
        });
        return;
      }
      if (!onRename) {
        setComposerNotice({
          tone: 'error',
          title: 'Could not rename this session',
          detail: 'This Sessions runtime does not expose durable rename yet.'
        });
        return;
      }
      try {
        await onRename(name);
        setText('');
        restoredDraftRef.current = null;
        setFeedback('sent');
        window.setTimeout(() => setFeedback('idle'), 500);
      } catch (reason) {
        setComposerNotice({
          tone: 'error',
          title: 'Could not rename this session',
          detail: reason instanceof Error ? reason.message : 'Try again after Sessions reconnects.'
        });
      }
      return;
    }

    if (richSession && trimmed.startsWith('/')) {
      const command = trimmed.split(/\s+/, 1)[0]?.toLowerCase() ?? 'slash command';
      setComposerNotice({
        tone: 'info',
        title: command === '/rc'
          ? 'Remote Control needs a Terminal session'
          : `${command} needs a Terminal session`,
        detail: command === '/rc'
          ? 'Claude exposes Remote Control only from its interactive terminal, and only when this machine has it turned on in Settings → Claude. This command was not sent as a chat message.'
          : 'Claude slash commands run in its interactive terminal. This command was not sent as a chat message.',
        canContinueInTerminal: Boolean(onContinueInTerminal),
        enableRemoteControl: command === '/rc'
      });
      return;
    }

    if (richSession && providerWorking && provider !== 'codex') {
      setComposerNotice({
        tone: 'info',
        title: 'Claude is still working',
        detail: 'Your draft is kept here and was not sent or queued. Send it when this turn finishes.'
      });
      return;
    }

    // Take the synchronous gate only when this invocation is actually about
    // to send. Validation-only slash-command paths above must leave the
    // composer usable.
    submitInFlightRef.current = true;
    setSubmitting(true);
    const submittedToActiveCodex = provider === 'codex' && providerWorking;
    try {
      if (text) {
        // The daemon owns text + Enter as one atomic operation. It still
        // writes two PTY frames (needed by Ink), but concurrent agents cannot
        // interleave their messages between those frames.
        await submitMessage('\x1b[200~' + text + '\x1b[201~');
      } else {
        // Empty buffer — just an Enter, e.g. to accept a y/n prompt.
        await send('\r');
      }
      if (text && onSubmitted) onSubmitted(text, submittedToActiveCodex);
      setText('');
      restoredDraftRef.current = null;
      setFeedback('sent');
      window.setTimeout(() => setFeedback('idle'), 500);
    } catch (reason) {
      setComposerNotice({
        tone: 'error',
        title: 'Message not sent',
        detail: reason instanceof Error
          ? `${reason.message} Your draft is still here.`
          : 'Sessions is reconnecting. Your draft is still here; send it again after the connection returns.'
      });
    } finally {
      setSubmitting(false);
      submitInFlightRef.current = false;
    }
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>): void => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      void submit();
      return;
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      if (text) {
        setText('');
      } else {
        void send('\x1b').catch(() => {
          setComposerNotice({
            tone: 'error',
            title: 'Key not sent',
            detail: 'Sessions is reconnecting. Try again when the connection returns.'
          });
        });
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

  // Paste handling. Cmd-V (screenshot from the system clipboard, or any
  // copied image) lands here as a clipboard paste
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
      {composerNotice ? (
        <div className={`input-composer-notice is-${composerNotice.tone}`} role={composerNotice.tone === 'error' ? 'alert' : 'status'}>
          <div>
            <strong>{composerNotice.title}</strong>
            <span>{composerNotice.detail}</span>
          </div>
          <div className="input-composer-notice-actions">
            {composerNotice.canContinueInTerminal && onContinueInTerminal ? (
              <button
                type="button"
                className="btn btn-secondary"
                disabled={providerWorking || continuingInTerminal}
                title={providerWorking ? 'Available after the current turn finishes' : 'End this Rich runtime, then continue the same Claude conversation in Terminal'}
                onClick={() => {
                  setContinuingInTerminal(true);
                  void onContinueInTerminal(Boolean(composerNotice.enableRemoteControl))
                    .catch((reason) => {
                      setComposerNotice({
                        tone: 'error',
                        title: 'Could not continue in Terminal',
                        detail: reason instanceof Error ? reason.message : 'The Rich session is still intact. Try again when it is idle.'
                      });
                    })
                    .finally(() => setContinuingInTerminal(false));
                }}
              >
                {providerWorking
                  ? 'Available when Claude finishes'
                  : continuingInTerminal
                    ? 'Preparing…'
                    : composerNotice.enableRemoteControl
                      // Says what the button does, in the order it happens.
                      // "Open Remote Control" implied this button turned it
                      // on; it does not — this machine's Settings choice does,
                      // and the handler checks it before ending anything.
                      ? 'End Rich & continue in Terminal with Remote Control…'
                      : 'End Rich & continue in Terminal…'}
              </button>
            ) : null}
            <button type="button" className="btn btn-ghost" onClick={() => setComposerNotice(null)}>Dismiss</button>
          </div>
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
          placeholder={sendAvailable
            ? `Message ${provider === 'codex' ? 'Codex' : 'Claude'} — Enter sends, Shift+Enter for newline`
            : 'This session is not connected, so messages cannot be sent.'}
          disabled={!sendAvailable || submitting}
          rows={Math.min(6, Math.max(1, text.split('\n').length))}
          autoCapitalize="sentences"
          autoCorrect="on"
          spellCheck
        />
        <div className="input-composer-footer">
          <button
            type="button"
            className="input-attach"
            disabled={!sendAvailable || uploading}
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
            onClick={() => void submit()}
            disabled={!sendAvailable || submitting}
            aria-label="Send"
            title={submitting ? 'Sending…' : 'Send (Enter)'}
          >
            <span aria-hidden>↑</span>
          </button>
        </div>
      </div>
    </div>
  );
}
