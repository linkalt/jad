/** Secret used only to authenticate browser/API requests. Never pass it to tools. */
export const API_TOKEN_ENV_NAME = "AGENT_API_TOKEN";

export function environmentWithoutApiToken(environment: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const safe = {...environment};
  delete safe[API_TOKEN_ENV_NAME];
  return safe;
}
