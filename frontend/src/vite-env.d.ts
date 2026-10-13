/// <reference types="vite/client" />

declare module 'virtual:catalog-schemes' {
  /** Every sample/schemes/*.json file, parsed, keyed by file name. */
  const schemes: Record<string, unknown>;
  export default schemes;
}
