import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';

import {toolGroupLayout} from './tool-group.ts';

const calls = ['a', 'b', 'c'].map(callId => ({callId}));

test('the first call is the header and the list holds only the calls after it', () => {
  const layout = toolGroupLayout(calls, undefined);

  assert.equal(layout.header.callId, 'a');
  assert.deepEqual(layout.listed.map(entry => entry.callId), ['b', 'c']);
  assert.equal(layout.holdsOpenPanel, false);
});

test('no call is both the header and in the list', () => {
  const layout = toolGroupLayout(calls, undefined);

  const shown = [layout.header, ...layout.listed].map(entry => entry.callId);
  assert.deepEqual(shown, ['a', 'b', 'c']);
  assert.equal(new Set(shown).size, calls.length);
});

test('two calls list exactly the second one', () => {
  const layout = toolGroupLayout(calls.slice(0, 2), undefined);

  assert.equal(layout.header.callId, 'a');
  assert.deepEqual(layout.listed.map(entry => entry.callId), ['b']);
});

test('the list is held open only while the panel shows a listed call', () => {
  assert.equal(toolGroupLayout(calls, 'tool-b').holdsOpenPanel, true);
  assert.equal(toolGroupLayout(calls, 'tool-c').holdsOpenPanel, true);
  // The header call is always on screen, so its panel holds nothing open.
  assert.equal(toolGroupLayout(calls, 'tool-a').holdsOpenPanel, false);
  assert.equal(toolGroupLayout(calls, 'tool-other').holdsOpenPanel, false);
  assert.equal(toolGroupLayout(calls, undefined).holdsOpenPanel, false);
});

test('calls added while streaming keep the header and extend the list', () => {
  const grown = toolGroupLayout([...calls, {callId: 'd'}], undefined);

  assert.equal(grown.header.callId, 'a');
  assert.deepEqual(grown.listed.map(entry => entry.callId), ['b', 'c', 'd']);
});

test('an empty group is refused', () => {
  assert.throws(() => toolGroupLayout([], undefined), /at least one call/);
});

// The renderer is a React component that needs the app's style compiler, so its
// wiring is pinned in the source: the header is the first call as a panel row,
// the wrench, count and chevron follow it in one left-aligned toggle, and only
// the listed calls are rendered below.
const source = readFileSync(new URL('./components/TranscriptParts.tsx', import.meta.url), 'utf8');
const row = source.slice(source.indexOf('function ToolGroupRow'), source.indexOf('function WebUiActivityGroup'));

test('the group header renders the first call and the list renders the rest', () => {
  assert.match(row, /renderCall\(header\.call, header\.result\)/);
  assert.match(row, /listed\.map\(\(\{call, result\}\) => renderCall\(call, result\)\)/);
  assert.doesNotMatch(row, /calls\.map\(\(\{call, result\}\) => renderCall/);
  assert.doesNotMatch(row, /calls\[calls\.length - 1\]/);
});

test('the icon group follows the header call instead of sitting at the far right', () => {
  assert.ok(row.indexOf('renderCall(header.call') < row.indexOf('icon="wrench"'));
  assert.ok(row.indexOf('icon="wrench"') < row.indexOf('{calls.length}'));
  assert.ok(row.indexOf('{calls.length}') < row.indexOf('icon="chevronDown"'));
  assert.doesNotMatch(row, /flex: 1/);
});

test('the toggle keeps its accessibility and keyboard behavior', () => {
  assert.match(row, /role="button"/);
  assert.match(row, /tabIndex=\{0\}/);
  assert.match(row, /aria-expanded=\{isExpanded\}/);
  assert.match(row, /aria-controls=\{listId\}/);
  assert.match(row, /aria-label=/);
  assert.match(row, /event\.key === 'Enter' \|\| event\.key === ' '/);
});
