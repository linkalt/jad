import {useCallback, useEffect, useRef, useState} from 'react';
import appConfig from './config.json';

type CopyState = 'idle' | 'copied' | 'error';

async function copyMarkdownToClipboard(value: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value);
    return;
  }

  const textarea = document.createElement('textarea');
  textarea.value = value;
  textarea.setAttribute('readonly', 'true');
  textarea.style.position = 'fixed';
  textarea.style.inset = '0';
  textarea.style.opacity = '0';
  document.body.appendChild(textarea);
  textarea.select();
  const copied = document.execCommand('copy');
  textarea.remove();
  if (!copied) {
    throw new Error('Clipboard access is unavailable');
  }
}

/** Copies `value` and reports `copied` or `error` for the configured feedback time. */
export function useCopyToClipboard(value: string): [CopyState, () => void] {
  const [copyState, setCopyState] = useState<CopyState>('idle');
  const resetTimerRef = useRef<number | null>(null);

  useEffect(() => {
    return () => {
      if (resetTimerRef.current !== null) {
        window.clearTimeout(resetTimerRef.current);
      }
    };
  }, []);

  const setTemporaryCopyState = useCallback((state: CopyState) => {
    if (resetTimerRef.current !== null) {
      window.clearTimeout(resetTimerRef.current);
    }
    setCopyState(state);
    resetTimerRef.current = window.setTimeout(() => {
      resetTimerRef.current = null;
      setCopyState('idle');
    }, appConfig.copy_feedback_duration_ms);
  }, []);

  const copy = useCallback(() => {
    void (async () => {
      try {
        await copyMarkdownToClipboard(value);
        setTemporaryCopyState('copied');
      } catch {
        setTemporaryCopyState('error');
      }
    })();
  }, [value, setTemporaryCopyState]);

  return [copyState, copy];
}

export function copyActionLabel(state: CopyState, noun: string): string {
  if (state === 'copied') {
    return `Copied ${noun}`;
  }
  if (state === 'error') {
    return 'Copy failed';
  }
  return `Copy ${noun}`;
}
