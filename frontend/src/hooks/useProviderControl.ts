import { useCallback, useEffect, useState } from 'react';
import { snapshot as fetchServerSnapshot } from '../api/sessionsd';
import {
  classifySnapshotComposerState,
  detectTrustChoice,
  type SnapshotComposerState,
  type TrustChoice
} from '../lib/detectMultiChoice';

interface Options {
  sessionId: string;
  terminalAvailable: boolean;
  // Changes when a composer send fails; the terminal is then re-read to
  // explain why.
  failedSendKey: string | null;
  // The daemon's durable needs-input line, when set. It arrives before any
  // send is attempted, so a control drawn at launch is recognised at once.
  needsInputDetail: string | null;
  sendRawInput?: (data: string) => void;
}

// Recognises a provider control on the terminal (a trust dialog, a picker, a
// confirmation) and offers a way to answer it from the conversation view.
// Rich sessions have no terminal stream, so nothing is classified for them.
export function useProviderControl({ sessionId, terminalAvailable, failedSendKey, needsInputDetail, sendRawInput }: Options) {
  const [blockingState, setBlockingState] = useState<SnapshotComposerState | null>(null);
  const [trustChoice, setTrustChoice] = useState<TrustChoice | null>(null);

  useEffect(() => {
    if (!terminalAvailable || (!failedSendKey && !needsInputDetail)) {
      setBlockingState(null);
      setTrustChoice(null);
      return;
    }
    let alive = true;
    const checkSnapshot = async (): Promise<void> => {
      try {
        const snap = await fetchServerSnapshot(sessionId);
        if (!alive) return;
        if (!snap) {
          setBlockingState(null);
          setTrustChoice(null);
          return;
        }
        const state = classifySnapshotComposerState(snap.text);
        setBlockingState(state.kind === 'normal-composer' ? null : state);
        setTrustChoice(state.kind === 'trust-prompt' ? detectTrustChoice(snap.text) : null);
      } catch {
        if (alive) {
          setBlockingState(null);
          setTrustChoice(null);
        }
      }
    };
    void checkSnapshot();
    return () => { alive = false; };
  }, [failedSendKey, needsInputDetail, sessionId, terminalAvailable]);

  // Arrow keys move the highlight; Enter confirms. A short gap between keys
  // lets the TUI redraw so the second key lands on the moved cursor.
  const answerControl = useCallback((keys: string[]) => {
    if (!sendRawInput) return;
    keys.forEach((key, index) => {
      window.setTimeout(() => sendRawInput(key), index * 60);
    });
  }, [sendRawInput]);

  return { blockingState, trustChoice, answerControl, controlPending: Boolean(needsInputDetail || blockingState) };
}
