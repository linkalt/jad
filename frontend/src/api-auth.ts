/* Keep the API secret out of the bundled configuration. A tab asks for it on
 * the first 401 and holds it only in sessionStorage. */
export function createAuthenticatedFetch(
  storageKey: string,
  transport: typeof globalThis.fetch = globalThis.fetch,
): typeof globalThis.fetch {
  let tokenPrompt: Promise<string | null> | undefined;

  const storedToken = (): string | null => {
    try {
      return globalThis.sessionStorage?.getItem(storageKey) ?? null;
    } catch {
      return null;
    }
  };
  const saveToken = (token: string | null): void => {
    try {
      if (token === null) globalThis.sessionStorage?.removeItem(storageKey);
      else globalThis.sessionStorage?.setItem(storageKey, token);
    } catch {
      // A private browsing tab may disallow storage; this request still uses the token.
    }
  };
  const askForToken = async (): Promise<string | null> => {
    if (typeof window === 'undefined' || typeof window.prompt !== 'function') return null;
    tokenPrompt ??= Promise.resolve().then(() => window.prompt('Enter the backend API token (AGENT_API_TOKEN):'))
      .finally(() => { tokenPrompt = undefined; });
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
    if (!token) return response;
    saveToken(token);
    response = await send(token);
    if (response.status === 401) saveToken(null);
    return response;
  };
}
