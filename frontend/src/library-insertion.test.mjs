import assert from 'node:assert/strict';
import test from 'node:test';

import {
  composerBadgeInsertionSeparator,
  composerInsertionSeparator,
  composerPlaceholderForDraft,
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

test('long-prompt and skill badges start on a new line without a blank spacer', () => {
  const existing = 'Keep this first line';
  for (const badgeValue of ['Full prompt instructions', 'Full skill instructions']) {
    assert.equal(composerBadgeInsertionSeparator(existing), '\n');
    assert.equal(
      `${existing}${composerBadgeInsertionSeparator(existing)}${badgeValue}`,
      `Keep this first line\n${badgeValue}`,
    );
  }
  assert.equal(composerBadgeInsertionSeparator(`${existing}\n`), '');
  assert.equal(composerBadgeInsertionSeparator(''), '');
  assert.equal(composerBadgeInsertionSeparator('  \n  '), '');
});

test('the empty composer keeps its placeholder but populated drafts hide it', () => {
  assert.equal(composerPlaceholderForDraft('', 'Ask agent'), 'Ask agent');
  assert.equal(composerPlaceholderForDraft('  \n  ', 'Ask agent'), 'Ask agent');
  assert.equal(composerPlaceholderForDraft('Review this change', 'Ask agent'), '');
});
