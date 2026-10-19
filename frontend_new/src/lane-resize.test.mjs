import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// One pane is sized by the two dividers on the conversation lane's edges: they
// are the only way to resize it. Both have to be visible before the pointer
// arrives — a grip that appears on hover cannot be found on an empty transcript,
// where there is no scrollbar to mark the edge — and their grab zones have to
// stay inside the frame whatever width the lane was last saved at.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const config = JSON.parse(
  readFileSync(new URL('./config.json', import.meta.url), 'utf8'),
);

/** The single `<ResizeHandle>` whose label is `label`. */
function laneHandle(label) {
  const at = app.indexOf(`label="${label}"`);
  assert.ok(at > 0, `no divider labelled ${label}`);
  const open = app.lastIndexOf('<ResizeHandle', at);
  const selfClosing = app.indexOf('/>', at);
  const withChildren = app.indexOf('</ResizeHandle>', at);
  assert.ok(
    selfClosing > 0 && (withChildren < 0 || selfClosing < withChildren),
    `${label} must have no children: Astryx draws its grip pill only when children are absent`,
  );
  return app.slice(open, selfClosing);
}

const labels = [
  'Resize conversation width from left border',
  'Resize conversation width from right border',
];

test('both lane dividers draw their grip at rest', () => {
  for (const label of labels) {
    const handle = laneHandle(label);
    assert.doesNotMatch(
      handle,
      /<VisuallyHidden/,
      `${label} passes children, which replace the grip pill`,
    );
    assert.doesNotMatch(
      handle,
      /isAlwaysVisible=\{false\}/,
      `${label} hides its grip until hovered`,
    );
  }
});

test('the frame reserves the grab width while the dividers are shown', () => {
  const frameAt = app.indexOf('ref={conversationFrameRef}');
  assert.ok(frameAt > 0, 'the frame is not measured');
  const padAt = app.indexOf(
    'paddingInline: appConfig.conversation_lane_grab_size',
    frameAt,
  );
  assert.ok(padAt > frameAt, 'the frame reserves no room for the grab zones');
  assert.ok(padAt - frameAt < 1500, 'the reservation belongs to the frame');
  // The reservation exists for the dividers, so it goes with them: with several
  // panes the row has to keep the whole area for the panes themselves.
  assert.match(
    app.slice(frameAt, padAt),
    /shownPaneKeys\.length === 1 && !isMobile/,
  );
});

test('the lane is saved no wider than the frame it renders in', () => {
  assert.match(
    app,
    /maxSize: pixel\(laneMaxPx\)/,
    'the lane keeps the configured maximum, so a saved width can exceed the frame',
  );
  const at = app.indexOf('const laneMaxPx =');
  assert.ok(at > 0, 'no lane ceiling');
  const rule = app.slice(at, app.indexOf('const conversation = useResizable', at));
  assert.match(rule, /conversationFramePx - 2 \* appConfig\.conversation_lane_grab_size/);
  // Below the minimum the ceiling would invert the clamp.
  assert.match(rule, /Math\.max\(\s*appConfig\.conversation_lane_min_size/);
  // The frame is what gets measured, and the lane's own group is not it: the
  // group is already clamped, so measuring it could never see the excess.
  assert.match(app, /ref=\{conversationFrameRef\}/);
});

test('the grab width is configuration, not a literal', () => {
  const grab = config.conversation_lane_grab_size;
  assert.equal(typeof grab, 'number', 'conversation_lane_grab_size must be a number');
  assert.ok(grab > 0 && Number.isInteger(grab), `conversation_lane_grab_size is ${grab}`);
  const uses = app.split('conversation_lane_grab_size').length - 1;
  assert.equal(uses, 2, 'the grab width is read by the frame padding and the lane ceiling');
});
