/** Keep the etag of a view's latest full snapshot, or forget it when the server sent none. */
export function rememberSnapshotEtag(
  etags: Map<string, string>,
  viewKey: string,
  etag: string | undefined,
): void {
  if (etag === undefined) {
    etags.delete(viewKey);
  } else {
    etags.set(viewKey, etag);
  }
}

/** Drop the etags of views whose caches were cleared, so their next fetch is unconditional. */
export function forgetSnapshotEtags(
  etags: Map<string, string>,
  viewKeys: Iterable<string>,
): void {
  for (const key of viewKeys) {
    etags.delete(key);
  }
}
