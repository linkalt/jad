import {useCallback, useState} from 'react';

export type ColorScheme = 'light' | 'dark';

export function parseColorScheme(raw: unknown): ColorScheme | undefined {
  return raw === 'light' || raw === 'dark' ? raw : undefined;
}

export function nextColorScheme(scheme: ColorScheme): ColorScheme {
  return scheme === 'dark' ? 'light' : 'dark';
}

/** The saved choice wins; otherwise the operating system's preference. */
export function resolveColorScheme(stored: unknown, prefersDark: boolean): ColorScheme {
  return parseColorScheme(stored) ?? (prefersDark ? 'dark' : 'light');
}

function readStored(key: string): unknown {
  try {
    return window.localStorage.getItem(key) ?? undefined;
  } catch {
    return undefined;
  }
}

function writeStored(key: string, scheme: ColorScheme): void {
  try {
    window.localStorage.setItem(key, scheme);
  } catch {
    // Ignored: the choice holds for this page when it cannot be saved.
  }
}

/** The page's color scheme; the Astryx `Theme` root applies it to `<html>`. */
export function useColorScheme(storageKey: string): [ColorScheme, () => void] {
  const [scheme, setScheme] = useState<ColorScheme>(() =>
    resolveColorScheme(readStored(storageKey), window.matchMedia('(prefers-color-scheme: dark)').matches),
  );
  const toggle = useCallback(() => {
    setScheme(current => {
      const next = nextColorScheme(current);
      writeStored(storageKey, next);
      return next;
    });
  }, [storageKey]);
  return [scheme, toggle];
}
