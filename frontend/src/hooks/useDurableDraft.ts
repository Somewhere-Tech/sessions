import { useCallback, useEffect, useRef, useState, type SetStateAction } from 'react';
import { draftStorageKey, readDraft, saveDraft } from '../lib/draftStore';

const WRITE_DELAY_MS = 300;

export function useDurableDraft(machineId: string, sessionId: string): {
  text: string;
  setText: (text: SetStateAction<string>) => void;
  clearAcknowledged: (submitted: string, submittedKey: string) => void;
  key: string;
  warning: string | null;
} {
  const key = draftStorageKey(machineId, sessionId);
  const [initial] = useState(() => readDraft(key));
  const [text, setTextState] = useState(initial.text);
  const [warning, setWarning] = useState<string | null>(initial.warning ?? null);
  const keyRef = useRef(key);
  const textRef = useRef(text);
  const dirtyRef = useRef(false);
  const timerRef = useRef<number | null>(null);

  const flush = useCallback(() => {
    if (timerRef.current !== null) window.clearTimeout(timerRef.current);
    timerRef.current = null;
    if (!dirtyRef.current) return;
    const result = saveDraft(keyRef.current, textRef.current);
    dirtyRef.current = !result.saved;
    setWarning(result.warning ?? null);
  }, []);

  const setText = useCallback((value: SetStateAction<string>) => {
    const next = typeof value === 'function' ? value(textRef.current) : value;
    textRef.current = next;
    dirtyRef.current = true;
    setTextState(next);
    setWarning(null);
    if (timerRef.current !== null) window.clearTimeout(timerRef.current);
    timerRef.current = window.setTimeout(flush, WRITE_DELAY_MS);
  }, [flush]);

  const clearAcknowledged = useCallback((submitted: string, submittedKey: string) => {
    if (keyRef.current !== submittedKey || textRef.current !== submitted) return;
    if (timerRef.current !== null) window.clearTimeout(timerRef.current);
    timerRef.current = null;
    textRef.current = '';
    dirtyRef.current = false;
    setTextState('');
    const result = saveDraft(submittedKey, '');
    setWarning(result.warning ?? null);
  }, []);

  useEffect(() => {
    if (keyRef.current !== key) {
      flush();
      keyRef.current = key;
      const restored = readDraft(key);
      textRef.current = restored.text;
      dirtyRef.current = false;
      setTextState(restored.text);
      setWarning(restored.warning ?? null);
    }
    const onPageHide = (): void => flush();
    window.addEventListener('pagehide', onPageHide);
    return () => {
      window.removeEventListener('pagehide', onPageHide);
      flush();
    };
  }, [flush, key]);

  return { text, setText, clearAcknowledged, key, warning };
}
