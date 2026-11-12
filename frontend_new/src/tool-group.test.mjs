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
  assert.match(row, /renderCall\(header\.call, header\.result, isExpanded \? undefined :/);
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

test('collapsed header expands before it can open a result', () => {
  const activity = source.slice(source.indexOf('function ActivityRow'), source.indexOf('export function ReasoningBlock'));
  assert.match(row, /isExpanded \? undefined : \{expand: \(\) => setIsToggled\(true\), listId\}/);
  assert.match(activity, /if \(expandGroup\) expandGroup\.expand\(\);\s*else control\?\.toggle\(detail, element\)/);
  assert.match(activity, /aria-controls=\{expandGroup\?\.listId\}/);
  assert.match(activity, /event\.key === 'Enter' \|\| event\.key === ' '/);
});

test('group and activity styles use semantic tokens and tight row spacing', () => {
  assert.match(source, /backgroundColor: colorVars\['--color-accent-muted'\]/);
  assert.doesNotMatch(source, /groupIcon/);
  assert.match(readFileSync(new URL('./index.css', import.meta.url), 'utf8'), /\.jad-group-header-call\[data-status='complete'\] svg \{ color: var\(--color-icon-purple\)/);
  assert.match(readFileSync(new URL('./index.css', import.meta.url), 'utf8'), /\.jad-activity-trigger:hover \{ background-color: var\(--color-overlay-hover\)/);
  assert.match(readFileSync(new URL('./index.css', import.meta.url), 'utf8'), /\.jad-activity-trigger:active \{ background-color: var\(--color-overlay-hover\)/);
  assert.match(row, /<VStack gap=\{0\} width="100%" xstyle=\{styles\.groupedActivity\}>/);
  assert.match(row, /<VStack gap=\{0\} width="100%">\s*\{listed\.map/);
});

// Render the actual ActivityRow and ToolGroupRow bodies under React in a DOM.
// Only Astryx's layout/icon wrappers and StyleX are stubbed; the state, event
// handlers, ARIA attributes, context and layout function are the real ones.
test('rendered group routes pointer and keyboard actions without opening a collapsed result', async () => {
  const {JSDOM} = await import('jsdom');
  const React = await import('react');
  const {createRoot} = await import('react-dom/client');
  const {transformWithOxc} = await import('vite');
  const {mkdirSync, writeFileSync, rmSync} = await import('node:fs');
  const {act} = React;
  const scratch = new URL('../node_modules/.cache/tool-group-dom-test/', import.meta.url);
  mkdirSync(scratch, {recursive: true});
  const activity = source.slice(source.indexOf('function ActivityRow('), source.indexOf('/** The row of a reasoning block'));
  const group = source.slice(source.indexOf('function ToolGroupRow('), source.indexOf('function WebUiActivityGroup('));
  assert.ok(activity.includes('function ActivityRow') && group.includes('function ToolGroupRow'));
  const harness = `
    import React, {useContext, useEffect, useId, useRef, useState} from 'react';
    import {toolGroupLayout} from ${JSON.stringify(new URL('./tool-group.ts', import.meta.url).href)};
    export const ActivityPanelContext = React.createContext(null);
    const activityRefsChanged = (a, b) => !a || a.length !== b.length || a.some((x, i) => x !== b[i]);
    const styles = {activityTrigger:{}, groupedActivity:{}, groupHeaderCall:{}, groupToggle:{}};
    const toolCallStatus = result => result ? (result.status === 'error' ? 'error' : 'complete') : 'running';
    const stylex = {props: () => ({})};
    const VStack = ({children}) => <div>{children}</div>;
    const HStack = ({children}) => <div>{children}</div>;
    const Icon = ({icon}) => <svg data-icon={icon} />;
    const Text = ({children}) => <span>{children}</span>;
    ${activity}
    ${group}
    export function Fixture() {
      const [detail, setDetail] = useState(null);
      const [opener, setOpener] = useState(null);
      const control = {
        openKey: detail?.key,
        toggle: (next, element) => {
          if (detail?.key === next.key) { setDetail(null); element.focus(); }
          else { setDetail(next); setOpener(element); }
        },
        update: () => {},
      };
      const calls = ['a', 'b', 'c'].map(id => ({call: {callId:id, name:'tool-'+id}, result:{status:'success'}}));
      const renderCall = (call, result, expandGroup) => <ActivityRow key={call.callId}
        detail={{key:'tool-'+call.callId, kind:'tool', name:call.name}} refs={[call.callId]} expandGroup={expandGroup}>
        <span>{call.name}</span></ActivityRow>;
      return <ActivityPanelContext.Provider value={control}>
        <ToolGroupRow calls={calls} renderCall={renderCall} />
        <output data-testid="panel">{detail?.key ?? 'closed'}</output>
        <button onClick={() => {setDetail(null); opener?.focus();}}>Close panel</button>
      </ActivityPanelContext.Provider>;
    }
  `;
  writeFileSync(new URL('fixture.mjs', scratch), (await transformWithOxc(harness, 'fixture.tsx', {jsx: {runtime:'automatic'}})).code);
  const dom = new JSDOM('<!doctype html><html><body><div id="root"></div></body></html>', {url:'http://localhost/'});
  const oldWindow = globalThis.window, oldDocument = globalThis.document, oldAct = globalThis.IS_REACT_ACT_ENVIRONMENT;
  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  let root;
  try {
    const {Fixture} = await import(new URL('fixture.mjs', scratch).href);
    root = createRoot(document.getElementById('root'));
    await act(() => root.render(React.createElement(Fixture)));
    const button = text => [...document.querySelectorAll('[role="button"]')].find(el => el.textContent.includes(text));
    const toggle = () => [...document.querySelectorAll("[aria-controls][aria-label]")].find(el => /^(Show|Hide) [0-9]+ more tool/.test(el.getAttribute("aria-label")));
    const panel = () => document.querySelector('[data-testid="panel"]').textContent;
    const click = el => act(() => el.dispatchEvent(new window.MouseEvent('click', {bubbles:true})));
    const key = (el, value) => act(() => el.dispatchEvent(new window.KeyboardEvent('keydown', {key:value, bubbles:true, cancelable:true})));
    assert.ok(button('tool-a').classList.contains('jad-activity-trigger'), 'the actual interactive row receives hover styling');
    assert.ok(!toggle().classList.contains('jad-activity-trigger'), 'the expand/count control has no hover background');
    assert.ok(!toggle().closest('.jad-activity-trigger'));
    const headerCall = document.querySelector('.jad-group-header-call');
    assert.equal(headerCall.getAttribute('data-status'), 'complete');
    assert.ok(headerCall.contains(button('tool-a')));
    const style = document.createElement('style');
    style.textContent = readFileSync(new URL('./index.css', import.meta.url), 'utf8').replace(/var\(--color-icon-purple\)/, 'rgb(1, 2, 3)');
    document.head.appendChild(style);
    headerCall.insertAdjacentHTML('beforeend', '<svg id="probe"></svg>');
    assert.equal(window.getComputedStyle(document.getElementById('probe')).color, 'rgb(1, 2, 3)', 'the first call status icon is recolored');
    assert.equal(window.getComputedStyle(toggle().querySelector('svg')).color === 'rgb(1, 2, 3)', false, 'the wrench/chevron are not recolored');
    assert.ok(button('tool-a').matches('.jad-activity-trigger'));
    document.getElementById('probe').remove();
    assert.equal(button('tool-a').getAttribute('tabindex'), '0');
    assert.equal(button('tool-a').getAttribute('aria-expanded'), 'false');
    assert.equal(button('tool-a').getAttribute('aria-label'), 'tool-a: Show more tool calls');
    assert.equal(toggle().getAttribute('aria-expanded'), 'false');
    assert.equal(button('tool-b'), undefined);
    assert.equal(document.getElementById(toggle().getAttribute('aria-controls')).hidden, true);
    assert.match(toggle().getAttribute('aria-label'), /Show 2 more tool calls/);
    const selection = window.getSelection();
    const range = document.createRange();
    range.selectNodeContents(button('tool-a').querySelector('span'));
    selection.removeAllRanges();
    selection.addRange(range);
    await click(button('tool-a'));
    assert.equal(toggle().getAttribute('aria-expanded'), 'false', 'selecting text does not expand');
    selection.removeAllRanges();
    await key(button('tool-a'), 'Escape');
    assert.equal(toggle().getAttribute('aria-expanded'), 'false');
    await click(button('tool-a'));
    assert.equal(panel(), 'closed');
    assert.equal(toggle().getAttribute('aria-expanded'), 'true');
    assert.equal(document.getElementById(toggle().getAttribute('aria-controls')).hidden, false);
    assert.ok(button('tool-b') && button('tool-c'));
    assert.equal([...document.querySelectorAll('[role="button"]')].filter(el => el.textContent.includes('tool-a')).length, 1);
    await click(button('tool-a'));
    assert.equal(panel(), 'tool-a');
    await click(button('tool-a'));
    assert.equal(panel(), 'closed');
    assert.equal(document.activeElement, button('tool-a'));
    await click(button('tool-b'));
    assert.equal(panel(), 'tool-b');
    await click(toggle());
    assert.equal(toggle().getAttribute('aria-expanded'), 'true', 'a listed panel keeps its opener visible');
    await click(button('tool-b'));
    assert.equal(panel(), 'closed');
    assert.equal(toggle().getAttribute('aria-expanded'), 'false', 'the deferred collapse takes effect when the panel closes');
    await key(button('tool-a'), 'Enter');
    assert.equal(panel(), 'closed');
    assert.equal(toggle().getAttribute('aria-expanded'), 'true');
    await key(button('tool-a'), ' ');
    assert.equal(panel(), 'tool-a');
    await key(button('tool-a'), 'Enter');
    assert.equal(panel(), 'closed');
    await key(toggle(), ' ');
    assert.equal(toggle().getAttribute('aria-expanded'), 'false');
    await key(toggle(), 'Enter');
    assert.equal(toggle().getAttribute('aria-expanded'), 'true');
    await key(button('tool-c'), ' ');
    assert.equal(panel(), 'tool-c');
  } finally {
    if (root) await act(() => root.unmount());
    dom.window.close();
    globalThis.window = oldWindow;
    globalThis.document = oldDocument;
    globalThis.IS_REACT_ACT_ENVIRONMENT = oldAct;
    rmSync(scratch, {recursive:true, force:true});
  }
});

// Check the actual shipped neutral theme, including alpha compositing of the
// hover/active tint over both grouped and ordinary rows in light and dark.
test('neutral theme tool rows retain text and icon contrast in each interaction state', () => {
  const css = readFileSync(new URL('./themes/neutral/theme.css', import.meta.url), 'utf8');
  const token = name => {
    const match = css.match(new RegExp(`--color-${name}: light-dark\\((#[0-9a-fA-F]{6,8}), (#[0-9a-fA-F]{6,8})\\)`));
    assert.ok(match, `missing neutral theme token ${name}`);
    return match.slice(1).map(hex => {
      const n = hex.slice(1);
      return [0, 2, 4, 6].map((i) => i === 6 && n.length === 6 ? 1 : parseInt(n.slice(i, i + 2), 16) / 255);
    });
  };
  const compose = (front, back) => front.slice(0, 3).map((v, i) => v * front[3] + back[i] * (1 - front[3])).concat(1);
  const luminance = rgb => rgb.slice(0, 3).map(c => c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4).reduce((sum, c, i) => sum + c * [0.2126, 0.7152, 0.0722][i], 0);
  const contrast = (a, b) => { const x = luminance(a), y = luminance(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
  const group = token('accent-muted'), surface = token('background-surface'), body = token('background-body');
  const secondary = token('text-secondary'), icon = token('icon-purple'), overlay = token('overlay-hover');
  for (const mode of [0, 1]) for (const background of [group, surface, body]) {
    for (const state of ['idle', 'hover', 'active']) {
      const bg = state === 'idle' ? background[mode] : compose(overlay[mode], background[mode]);
      assert.ok(contrast(secondary[mode], bg) >= 4.5, `secondary text: mode ${mode}, ${state}`);
      if (background === group) assert.ok(contrast(icon[mode], bg) >= 3, `group icon: mode ${mode}, ${state}`);
    }
  }
});

// Exercise CSS parsing/minification, not just the TSX style declaration. The
// DOM test above checks that these selectors reach the interactive elements.
test('compiled activity CSS paints hover and press on the interactive row', async () => {
  const {transform} = await import('lightningcss');
  const css = readFileSync(new URL('./index.css', import.meta.url));
  const compiled = transform({filename: 'index.css', code: css, minify: true}).code.toString();
  assert.match(compiled, /\.jad-activity-trigger:hover\{[^}]*background-color:var\(--color-overlay-hover\)/);
  assert.match(compiled, /\.jad-activity-trigger:active\{[^}]*background-color:var\(--color-overlay-hover\)/);
  assert.match(compiled, /box-shadow:inset/);
  assert.match(compiled, /\.jad-group-header-call\[data-status=complete\] svg\{color:var\(--color-icon-purple\)\}/);
});
