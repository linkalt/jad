/**
 * Catches the composer's DOM diverging from React's copy of it.
 *
 * React owns the content it portals into each badge's `span[data-astryx-token]`,
 * while the badge spans themselves and the draft's text nodes are edited by hand
 * — the composer's own helpers, and the browser. When a React-owned node loses
 * its parent while its badge is still on screen, React's next deletion pass
 * throws `NotFoundError` from `removeChild`, and a commit-phase error costs React
 * the whole root: the blank page bug report 0005 describes.
 *
 * No user path has reproduced that yet, so this watches for the divergence
 * itself — it is detectable while the app is still alive — and records the
 * composer operation that ran last. Dev builds only: nothing installs it in a
 * production bundle, and `noteComposerOperation` costs a no-op there.
 */

export type ComposerDesyncKind = 'orphaned-react-node' | 'removeChild-mismatch';

export interface ComposerDesync {
  kind: ComposerDesyncKind;
  /** What happened, in one line. */
  detail: string;
  /** The composer operation that ran last when it happened. */
  breadcrumb: string | null;
  at: number;
}

/** The event the app listens for, so the evidence reaches the notifications. */
export const COMPOSER_DESYNC_EVENT = 'jad:composer-desync';

/** How many composer operations are kept as breadcrumbs. */
export const BREADCRUMB_LIMIT = 8;

/** Newest first, capped: the last operations before a divergence. */
export function pushBreadcrumb(
  current: readonly string[],
  name: string,
  limit: number = BREADCRUMB_LIMIT,
): readonly string[] {
  return [name, ...current].slice(0, Math.max(0, limit));
}

/**
 * Whether a removal is the divergence React cannot survive.
 *
 * A React-owned node leaving a badge whose span is still on screen: React still
 * believes the node is in the container, and removing it again is what throws.
 * A removal from a *detached* badge is React unmounting that badge itself — the
 * only way it is supposed to go.
 */
export function shouldFlagRemoval(input: {
  removedCarriesReact: boolean;
  containerIsTokenSpan: boolean;
  containerConnected: boolean;
}): boolean {
  return input.removedCarriesReact && input.containerIsTokenSpan && input.containerConnected;
}

/** One line of evidence for the console and the notification. */
export function describeDesync(desync: ComposerDesync): string {
  const crumb = desync.breadcrumb === null
    ? 'no composer operation recorded'
    : `last composer operation: ${desync.breadcrumb}`;
  return `${desync.kind} — ${desync.detail} (${crumb})`;
}

/** `Node.ELEMENT_NODE`, spelled out so the rule holds without a DOM. */
const ELEMENT_NODE = 1;

/**
 * Whether a node is one React created.
 *
 * React brands every host element it renders with `__reactFiber$…` /
 * `__reactProps$…`, and nothing else does. Text nodes never carry a marker,
 * which is why the composer's text-node edits are safe by construction: the
 * editable's textual content is written imperatively, and the only React-owned
 * nodes in a badge live *inside* its token span — a level no helper touches.
 */
export function carriesReactInternals(node: Node): boolean {
  return node.nodeType === ELEMENT_NODE
    && Object.keys(node).some(key => key.startsWith('__react'));
}

let breadcrumbs: readonly string[] = [];
let installed = false;

/** Records a composer operation for the next report. No-op until installed. */
export function noteComposerOperation(name: string): void {
  if (!installed) {
    return;
  }
  breadcrumbs = pushBreadcrumb(breadcrumbs, name);
}

/** The last composer operations, newest first. */
export function composerBreadcrumbs(): readonly string[] {
  return breadcrumbs;
}

/** Vite replaces `import.meta.env` at build time; the flag is the only field read. */
const isDevBuild = (import.meta as ImportMeta & {env?: {DEV?: boolean}}).env?.DEV === true;

/** Whether this build should watch for the divergence. */
export function shouldInstallDesyncWatch(enabledInConfig: boolean): boolean {
  return enabledInConfig && isDevBuild;
}

/**
 * Installs the watch and returns the disposer.
 *
 * Two detectors: a mutation observer for React-owned nodes leaving a badge that
 * is still on screen, and a `removeChild` recorder for the throw itself — the
 * exact container, child and stack the report needs. Idempotent, because a
 * StrictMode mount runs its effects twice.
 */
export function installDesyncWatch(doc: Document = document): () => void {
  if (installed) {
    return () => undefined;
  }
  installed = true;

  const report = (desync: ComposerDesync): void => {
    console.warn('[composer-desync]', describeDesync(desync), desync);
    doc.dispatchEvent(new CustomEvent<ComposerDesync>(COMPOSER_DESYNC_EVENT, {detail: desync, bubbles: true}));
  };
  const tokenAncestorOf = (node: Node): Element | null => {
    for (let current: Node | null = node; current !== null; current = current.parentNode) {
      if (current.nodeType === Node.ELEMENT_NODE
        && (current as Element).hasAttribute('data-astryx-token')) {
        return current as Element;
      }
    }
    return null;
  };

  const observer = new MutationObserver(records => {
    for (const record of records) {
      if (record.type !== 'childList' || record.removedNodes.length === 0) {
        continue;
      }
      const container = tokenAncestorOf(record.target);
      if (container === null || !container.isConnected) {
        continue;
      }
      for (const removed of Array.from(record.removedNodes)) {
        const flagged = shouldFlagRemoval({
          removedCarriesReact: carriesReactInternals(removed)
            || (removed instanceof Element
              && Array.from(removed.querySelectorAll('*')).some(carriesReactInternals)),
          containerIsTokenSpan: container.hasAttribute('data-astryx-token'),
          containerConnected: container.isConnected,
        });
        if (flagged) {
          report({
            kind: 'orphaned-react-node',
            detail: `${removed.nodeName.toLowerCase()} left badge <${container.getAttribute('data-astryx-token')?.slice(0, 24)}…> while it is on screen`,
            breadcrumb: breadcrumbs[0] ?? null,
            at: Date.now(),
          });
        }
      }
    }
  });
  observer.observe(doc, {childList: true, subtree: true});

  const originalRemoveChild = Node.prototype.removeChild;
  Node.prototype.removeChild = function patchedRemoveChild<T extends Node>(this: Node, child: T): T {
    if (child.parentNode !== this) {
      report({
        kind: 'removeChild-mismatch',
        detail: `removeChild(<${this.nodeName.toLowerCase()}>, <${child.nodeName.toLowerCase()}>) but the child's parent is <${child.parentNode?.nodeName.toLowerCase() ?? 'null'}>`,
        breadcrumb: breadcrumbs[0] ?? null,
        at: Date.now(),
      });
    }
    return originalRemoveChild.call(this, child) as T;
  };

  return () => {
    installed = false;
    breadcrumbs = [];
    observer.disconnect();
    Node.prototype.removeChild = originalRemoveChild;
  };
}
