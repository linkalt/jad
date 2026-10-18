/**
 * Read the token from the URL fragment (`#<paramName>=<token>`) and remove it
 * from the address bar. A fragment is never sent to the server, and the token
 * is dropped from the current history entry so it is not left in the page
 * URL. Returns null, leaving the URL untouched, when the parameter is absent.
 * This is the way in for contexts where window.prompt is blocked.
 */
export function takeTokenFromFragment(
  paramName: string,
  location: Pick<Location, 'hash' | 'pathname' | 'search'> | undefined = globalThis.location,
  history: Pick<History, 'replaceState' | 'state'> | undefined = globalThis.history,
): string | null {
  if (!location || !location.hash) return null;
  const params = new URLSearchParams(location.hash.slice(1));
  const token = params.get(paramName)?.trim();
  if (!token) return null;
  params.delete(paramName);
  const rest = params.toString();
  try {
    history?.replaceState(history.state, '', `${location.pathname}${location.search}${rest ? `#${rest}` : ''}`);
  } catch {
    // Without history access the token stays in the address bar; it is still used.
  }
  return token;
}

/** The built-in way to ask for the token: the browser's own prompt, when it has one. */
export function promptForToken(): Promise<string | null> {
  if (typeof window === 'undefined' || typeof window.prompt !== 'function') return Promise.resolve(null);
  return Promise.resolve(window.prompt('Enter the backend API token (JAD_API_TOKEN):'));
}

/* Keep the API secret out of the bundled configuration. A tab takes it from
 * the URL fragment when one is given, otherwise asks for it on the first 401,
 * and holds it only in sessionStorage (and in memory if storage is blocked). */
export function createAuthenticatedFetch(
  storageKey: string,
  transport: typeof globalThis.fetch = globalThis.fetch,
  fragmentParam?: string,
  requestToken: () => Promise<string | null> = promptForToken,
): typeof globalThis.fetch {
  let tokenPrompt: Promise<string | null> | undefined;
  let heldToken: string | null = null;

  const storedToken = (): string | null => {
    try {
      return globalThis.sessionStorage?.getItem(storageKey) ?? heldToken;
    } catch {
      return heldToken;
    }
  };
  const saveToken = (token: string | null): void => {
    heldToken = token;
    try {
      if (token === null) globalThis.sessionStorage?.removeItem(storageKey);
      else globalThis.sessionStorage?.setItem(storageKey, token);
    } catch {
      // A private browsing tab may disallow storage; the in-memory copy keeps it usable.
    }
  };
  const fragmentToken = fragmentParam ? takeTokenFromFragment(fragmentParam) : null;
  if (fragmentToken) saveToken(fragmentToken);
  if (fragmentParam) {
    // Adding a fragment to a page that is already open is a same-document
    // navigation: nothing reloads, so the token has to be taken on the event.
    globalThis.addEventListener?.('hashchange', () => {
      const added = takeTokenFromFragment(fragmentParam);
      if (added) saveToken(added);
    });
  }
  const askForToken = async (): Promise<string | null> => {
    tokenPrompt ??= Promise.resolve().then(requestToken).finally(() => { tokenPrompt = undefined; });
    return tokenPrompt;
  };

  return async (input, init = {}) => {
    const send = (token: string | null) => transport(input, {
      ...init,
      headers: {
        ...Object.fromEntries(new Headers(init.headers).entries()),
        ...(token ? {Authorization: `Bearer ${token}`} : {}),
      },
    });
    const attemptedToken = storedToken();
    let response = await send(attemptedToken);
    if (response.status !== 401) return response;
    const newerToken = storedToken();
    const token = newerToken && newerToken !== attemptedToken
      ? newerToken
      : (await askForToken())?.trim() || null;
    if (!token) {
      // The server rejected the stored token and no replacement was given:
      // drop it, unless a concurrent request already replaced it.
      if (attemptedToken && storedToken() === attemptedToken) saveToken(null);
      return response;
    }
    saveToken(token);
    response = await send(token);
    if (response.status === 401) saveToken(null);
    return response;
  };
}
