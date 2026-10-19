import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// With one pane on screen the width the user drags is the *composer's*, and the
// dividers sit on its two sides — the layout `frontend/` shipped. The transcript
// around it keeps the whole window, so its scrollbar reaches the outer edge
// instead of stopping at a lane, and the dividers are always beside something
// visible rather than an invisible line in empty space.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const pane = readFileSync(
  new URL('./components/ViewPane.tsx', import.meta.url),
  'utf8',
);
const config = JSON.parse(
  readFileSync(new URL('./config.json', import.meta.url), 'utf8'),
);

/** One `<ResizeHandle>` element, located by the label it carries. */
function handleIn(source, label) {
  const at = source.indexOf(`label="${label}"`);
  assert.ok(at > 0, `no divider labelled ${label}`);
  const open = source.lastIndexOf('<ResizeHandle', at);
  const selfClosing = source.indexOf('/>', at);
  const withChildren = source.indexOf('</ResizeHandle>', at);
  assert.ok(
    selfClosing > 0 && (withChildren < 0 || selfClosing < withChildren),
    `${label} must have no children: Astryx draws its grip pill only when children are absent`,
  );
  return source.slice(open, selfClosing);
}

const labels = [
  'Resize conversation width from left border',
  'Resize conversation width from right border',
];

test('the single pane keeps the window width for its transcript', () => {
  // The group is the transcript's box: narrowing it to the lane is what put the
  // scrollbar in the middle of the window.
  assert.match(app, /width="100%"[\s\S]{0,400}data-pane-group=""/);
  assert.doesNotMatch(
    app,
    /shownPaneKeys\.length === 1\s*\n?\s*\? conversation\.size/,
    'the lane must not size the pane group again',
  );
  assert.doesNotMatch(
    app,
    /paddingInline: appConfig\.conversation_lane_grab_size/,
    'no reservation is needed: the dividers sit beside the composer now',
  );
});

test('the composer carries the lane, with a divider on each side', () => {
  // Only one pane, and only on a desktop: a phone has no room to drag a lane and
  // several panes size themselves.
  assert.match(
    app,
    /composerLane=\{isSinglePane && !isMobile && index === 0/,
    'the lane is handed to the single pane only',
  );
  assert.match(
    pane,
    /composerLane === undefined \? \(\s*composerNode\s*\)/,
    'several panes leave the composer at the pane width',
  );
  assert.match(pane, /width=\{composerLane\.widthPx\} maxWidth="100%"/);
  for (const label of labels) {
    const handle = handleIn(pane, label);
    assert.match(handle, /resizable=\{composerLane\.resize\}/);
    assert.doesNotMatch(handle, /isAlwaysVisible=\{false\}/, `${label} hides its grip`);
  }
});

test('the composer lane is saved no wider than the window', () => {
  assert.match(app, /maxSize: pixel\(laneMaxPx\)/);
  const at = app.indexOf('const laneMaxPx =');
  assert.ok(at > 0, 'no lane ceiling');
  const rule = app.slice(at, app.indexOf('const conversation = useResizable', at));
  assert.match(rule, /conversationFramePx - 2 \* appConfig\.conversation_lane_grab_size/);
  assert.match(rule, /Math\.max\(\s*appConfig\.conversation_lane_min_size/);
  assert.match(app, /ref=\{conversationFrameRef\}/);
});

test('the grab width is configuration, not a literal', () => {
  const grab = config.conversation_lane_grab_size;
  assert.equal(typeof grab, 'number', 'conversation_lane_grab_size must be a number');
  assert.ok(grab > 0 && Number.isInteger(grab), `conversation_lane_grab_size is ${grab}`);
  assert.match(app, /appConfig\.conversation_lane_grab_size/);
});

test('the conversation follows the composer, not the window', () => {
  // The scroll area spans the pane, so the scrollbar is on the window edge; the
  // column inside it is the lane, so the transcript moves with the divider.
  assert.match(
    pane,
    /width=\{composerLane === undefined \? '100%' : composerLane\.widthPx\}/,
    'the transcript column must take the composer width',
  );
  assert.match(
    pane,
    /maxWidth="100%"\s*\n\s*height="100%"\s*\n\s*xstyle=\{styles\.conversationLane\}/,
    'a lane wider than the pane still has to fit',
  );
});
