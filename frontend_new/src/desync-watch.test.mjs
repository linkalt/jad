import assert from 'node:assert/strict';
import test from 'node:test';

import {
  carriesReactInternals,
  describeDesync,
  pushBreadcrumb,
  shouldFlagRemoval,
  shouldInstallDesyncWatch,
} from './desync-watch.ts';

test('breadcrumbs keep the newest operation first, capped', () => {
  let crumbs = [];
  for (const name of ['a', 'b', 'c']) crumbs = pushBreadcrumb(crumbs, name, 2);
  assert.deepEqual(crumbs, ['c', 'b']);
  assert.deepEqual(pushBreadcrumb([], 'one', 0), []);
});

test('flags only a React-owned node leaving a badge that is still on screen', () => {
  const base = {removedCarriesReact: true, containerIsTokenSpan: true, containerConnected: true};
  assert.equal(shouldFlagRemoval(base), true);
  // A text node is the composer's own to remove.
  assert.equal(shouldFlagRemoval({...base, removedCarriesReact: false}), false);
  // React unmounting a badge removes its children from the detached span: expected.
  assert.equal(shouldFlagRemoval({...base, containerConnected: false}), false);
  // The editable itself is not a badge container.
  assert.equal(shouldFlagRemoval({...base, containerIsTokenSpan: false}), false);
});

test('report names the operation that ran last', () => {
  const light = describeDesync({kind: 'orphaned-react-node', detail: 'span left badge', breadcrumb: 'endLineAtCursor', at: 0});
  assert.match(light, /orphaned-react-node — span left badge/);
  assert.match(light, /last composer operation: endLineAtCursor/);
  const bare = describeDesync({kind: 'removeChild-mismatch', detail: 'x', breadcrumb: null, at: 0});
  assert.match(bare, /no composer operation recorded/);
});

test('recognizes React-owned elements and nothing else', () => {
  // What a React host element looks like (measured live: 2 markers on the node
  // React portals into a badge, 0 on Astryx's own token span).
  assert.equal(carriesReactInternals({nodeType: 1, __reactFiber$abc: {}, __reactProps$abc: {}}), true);
  assert.equal(carriesReactInternals({nodeType: 1, className: 'badge'}), false);
  // A text node has no marker to find — and React renders nothing into the
  // editable, so no text node is ever React's to protect.
  assert.equal(carriesReactInternals({nodeType: 3, __reactFiber$abc: {}}), false);
});

test('never installs without the dev build', () => {
  // The test runtime has no `import.meta.env.DEV`, which is what a production
  // bundle resolves to as well: the flag alone must not install it.
  assert.equal(shouldInstallDesyncWatch(true), false);
  assert.equal(shouldInstallDesyncWatch(false), false);
});
