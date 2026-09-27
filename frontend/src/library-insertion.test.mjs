import assert from 'node:assert/strict';
import test from 'node:test';

import {
  composerBadgeInsertionPlan,
  composerBadgeSharesLine,
  composerBadgeStartsDraft,
  composerCaretAfterGlue,
  composerInsertionSeparator,
  composerPlaceholderForDraft,
  composerTail,
  composerTextInsertionPlan,
  promptNeedsBadge,
} from './library-insertion.ts';

test('short prompts remain visible and prompts over Astryx configured threshold use a badge', () => {
  assert.equal(promptNeedsBadge(1_999, true, 2_000), false);
  assert.equal(promptNeedsBadge(2_000, true, 2_000), false);
  assert.equal(promptNeedsBadge(2_001, true, 2_000), true);
  assert.equal(promptNeedsBadge(2_001, false, 2_000), false);
});

test('inserting into an empty composer adds no separator', () => {
  assert.equal(composerInsertionSeparator(''), '');
  assert.equal(composerInsertionSeparator('  \n  '), '');
});

test('inserting after existing text adds only enough line breaks to separate it', () => {
  assert.equal(composerInsertionSeparator('draft'), '\n\n');
  assert.equal(composerInsertionSeparator('draft\n'), '\n');
  assert.equal(composerInsertionSeparator('draft\n\n'), '');
});

test('text prompts and templates preserve the existing first line and every inserted line', () => {
  const existing = 'Keep this first line';
  for (const inserted of ['# Prompt\nPrompt body', '# Template\nTemplate body']) {
    const result = `${existing}${composerInsertionSeparator(existing)}${inserted}`;
    assert.equal(result, `Keep this first line\n\n${inserted}`);
  }
});

test('long-prompt and skill badges take a line of their own, with the caret below', () => {
  const existing = 'Keep this first line';
  const plan = composerBadgeInsertionPlan('content');
  assert.deepEqual(plan, {before: '\n', endsLine: true, replacesDraft: false});
  assert.equal(
    `${existing}${plan.before}Full prompt instructions`,
    'Keep this first line\nFull prompt instructions',
  );
  // A draft that already ends on a line break gets no second spacer before the badge.
  assert.deepEqual(composerBadgeInsertionPlan('line-break'), {before: '', endsLine: true, replacesDraft: false});
  // An empty draft keeps the placeholder break out of the badge's way.
  assert.deepEqual(composerBadgeInsertionPlan('empty'), {before: '', endsLine: true, replacesDraft: true});
});

test('a badge whose value ends with a newline is not a line break', () => {
  // Every library file ends with a newline and a badge serializes as the whole
  // file, so the serialized draft of a draft ending in a badge reads as "...\n".
  // Trusting that left the next badge sharing the previous badge's line.
  assert.equal(composerTail({kind: 'token', text: ''}, true), 'content');
  assert.equal(composerTail({kind: 'text', text: '\u00A0'}, true), 'content');
});

test('the composer tail separates an empty draft, a line break and real content', () => {
  assert.equal(composerTail(null, false), 'empty');
  // Clearing the composer leaves the browser's placeholder break behind.
  assert.equal(composerTail({kind: 'br', text: ''}, false), 'empty');
  assert.equal(composerTail({kind: 'text', text: '  \n  '}, false), 'empty');
  assert.equal(composerTail({kind: 'br', text: ''}, true), 'line-break');
  assert.equal(composerTail({kind: 'text', text: 'a line\n'}, true), 'line-break');
  assert.equal(composerTail({kind: 'text', text: 'a line'}, true), 'content');
  assert.equal(composerTail(null, true), 'content');
});

test('a caret counts as a badge’s only across the badge’s own spacer and line', () => {
  // Shift+Enter pushes a badge down, so the composer has to tell the positions
  // the browser leaves beside a token from a caret the user put in their text.
  // Glue after a badge, the badge's own break, an empty node — all spare spacer.
  assert.equal(composerCaretAfterGlue(''), true);
  assert.equal(composerCaretAfterGlue('\u00A0'), true);
  assert.equal(composerCaretAfterGlue('\n'), true);
  assert.equal(composerCaretAfterGlue('\u00A0\n\n'), true);
  // The user's own words between the caret and the badge end its claim on it.
  assert.equal(composerCaretAfterGlue('user messages'), false);
  assert.equal(composerCaretAfterGlue('  note'), false);
  assert.equal(composerCaretAfterGlue('\nnote'), false);
});

test('a badge moves while it still has to make room on its line', () => {
  const line = (text, hasInlineBefore = false, hasBreak = false) => ({text, hasInlineBefore, hasBreak});
  // A badge that shares its line with text or another badge moves down.
  assert.equal(composerBadgeSharesLine(line('user messages')), true);
  assert.equal(composerBadgeSharesLine(line('', true)), true);
  // A badge that already owns its line does not move again.
  assert.equal(composerBadgeSharesLine(line('', false, true)), false);
  assert.equal(composerBadgeSharesLine(line('   ')), false);
});

test('a badge still moves down while it starts the draft', () => {
  const line = (text, hasInlineBefore = false, hasBreak = false) => ({text, hasInlineBefore, hasBreak});
  // Just inserted: the badge owns the first line, so Shift+Enter makes room above.
  assert.equal(composerBadgeStartsDraft(line('')), true);
  assert.equal(composerBadgeStartsDraft(line('\u00A0')), true);
  // Moved once already — there is now a line above it — so it stays put.
  assert.equal(composerBadgeStartsDraft(line('', false, true)), false);
  assert.equal(composerBadgeStartsDraft(line('user messages')), false);
  assert.equal(composerBadgeStartsDraft(line('', true)), false);
});

test('text inserts keep their blank-line separator and leave nothing after the text', () => {
  assert.deepEqual(composerTextInsertionPlan('Keep this first line', 'content'), {
    before: '\n\n',
    endsLine: false,
    replacesDraft: false,
  });
  assert.deepEqual(composerTextInsertionPlan('Keep this first line\n', 'line-break'), {
    before: '\n',
    endsLine: false,
    replacesDraft: false,
  });
  assert.deepEqual(composerTextInsertionPlan('', 'empty'), {
    before: '',
    endsLine: false,
    replacesDraft: true,
  });
});

test('the empty composer keeps its placeholder but populated drafts hide it', () => {
  assert.equal(composerPlaceholderForDraft('', 'Ask agent'), 'Ask agent');
  assert.equal(composerPlaceholderForDraft('  \n  ', 'Ask agent'), 'Ask agent');
  assert.equal(composerPlaceholderForDraft('Review this change', 'Ask agent'), '');
});
