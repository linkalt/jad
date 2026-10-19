import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// A user message is a bubble whose cap comes from Astryx:
// `max-width: max(80%, 280px)`, a percentage of the column it sits in. That
// percentage needs a column with a definite width — inside a shrink-to-fit
// column it is circular, so the cap settles on the width of the pane the row
// was first laid out in and the bubble stops following the pane afterwards
// (measured: 502px kept while the lane went 728 → 937 and the column 627 → 840).
const row = readFileSync(
  new URL('./components/TranscriptRow.tsx', import.meta.url),
  'utf8',
);
const config = JSON.parse(
  readFileSync(new URL('./config.json', import.meta.url), 'utf8'),
);

const userBranch = row.slice(
  row.indexOf("message.sender === 'user'"),
  row.indexOf('return <AssistantMessage'),
);

test('the user column has a definite width for the bubble cap', () => {
  assert.match(
    userBranch,
    /<VStack align="end" gap=\{0\.5\} style=\{\{flex: '1 1 0%', minWidth: 0\}\}>/,
    'the column must take the row, or the 80% cap freezes at the first layout',
  );
  // The old shrink-to-fit column, which is what froze the width.
  assert.doesNotMatch(
    userBranch,
    /<VStack align="end" gap=\{0\.5\}>/,
    'a shrink-to-fit column makes the cap circular again',
  );
});

test('the bubble keeps its responsive cap instead of a measured size', () => {
  // A pixel width (or a JS measurement) would freeze the bubble the same way.
  const bubble = userBranch.slice(
    userBranch.indexOf('<ChatMessageBubble'),
    userBranch.indexOf('</ChatMessageBubble>'),
  );
  assert.ok(bubble.length > 0, 'no bubble in the user row');
  assert.doesNotMatch(bubble, /width=\{|style=\{\{width/, 'the bubble sets its own width');
  assert.match(bubble, /contentWidth="100%"/, 'the markdown fills the bubble');
});

test('a user bubble is never narrower than the configured minimum', () => {
  const min = config.user_bubble_min_width;
  assert.equal(typeof min, 'number', 'user_bubble_min_width must be a number');
  assert.ok(Number.isInteger(min) && min > 0, `user_bubble_min_width is ${min}`);
  // `min(100%, max(80%, N))` keeps Astryx's 80% cap while the column can afford
  // N, holds the bubble at N just above that, and turns the cap into the whole
  // column — the assistant rows' width — once the column is no wider than N.
  assert.match(
    row,
    /maxWidth: `min\(100%, max\(80%, \$\{appConfig\.user_bubble_min_width\}px\)\)`/,
    'the cap must be built from configuration, not from a literal',
  );
  assert.match(
    userBranch,
    /<ChatMessageBubble style=\{userBubbleStyle\}>/,
    'the bubble must carry the cap',
  );
});
