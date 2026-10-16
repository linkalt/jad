import {promptForToken} from './api-auth.ts';

/** Shows a masked token field; resolves with what was typed, or null when dismissed. */
export type TokenPresenter = (resolve: (token: string | null) => void) => void;

let presenter: TokenPresenter | null = null;

/**
 * Register the UI that asks for the token. Returns the unregister function; only
 * the latest registration is used. Without one, the request falls back to the
 * browser prompt.
 */
export function registerTokenPresenter(next: TokenPresenter): () => void {
  presenter = next;
  return () => {
    if (presenter === next) presenter = null;
  };
}

export function requestApiToken(): Promise<string | null> {
  const active = presenter;
  if (!active) return promptForToken();
  return new Promise(resolve => active(resolve));
}
