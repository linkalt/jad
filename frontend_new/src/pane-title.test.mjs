import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

import appConfig from './config.json' with {type: 'json'};
import {paneTitleParts, paneTitleText, truncatePaneTitlePart} from './pane-title.ts';

const pane = readFileSync(new URL('./components/ViewPane.tsx', import.meta.url), 'utf8');

test('the strip names the conversation; profile, provider, model and effort are in its tooltip', () => {
  assert.match(pane, /const sessionLabel = titleParts\[0\]\?\.label \?\? label;/, 'the strip takes the session part');
  assert.match(pane, /\{sessionLabel\}/, 'and renders only that');
  assert.equal(pane.includes('titleParts.map('), false, 'the parts are no longer laid out inline');
  assert.match(
    pane,
    /const detailText = titleParts\.slice\(1\)\.map\(part => part\.label\)\.join\(' · '\);/,
    'the tooltip holds exactly the parts the strip dropped',
  );
  assert.match(
    pane,
    /<Tooltip content=\{detailText\} placement="below" isEnabled=\{detailText !== ''\}>/,
    'no bubble when there is no detail',
  );
});

test('a pane title names the session, the profile, the provider, the model and the effort', () => {
  const parts = paneTitleParts({
    sessionTitle: 'Atlas onboarding flow',
    profile: 'reviewer',
    provider: 'anthropic',
    model: 'claude-sonnet-5',
    effort: 'high',
  });
  assert.deepEqual(parts, [
    {key: 'session', label: 'Atlas onboarding flow'},
    {key: 'profile', label: 'reviewer'},
    {key: 'provider', label: 'anthropic'},
    {key: 'model', label: 'claude-sonnet-5'},
    {key: 'effort', label: 'high'},
  ]);
  assert.equal(
    paneTitleText({
      sessionTitle: 'Atlas onboarding flow',
      profile: 'reviewer',
      provider: 'anthropic',
      model: 'claude-sonnet-5',
      effort: 'high',
    }),
    'Atlas onboarding flow · reviewer · anthropic · claude-sonnet-5 · high',
  );
});

test('unknown parts are left out rather than shown as placeholders', () => {
  assert.deepEqual(paneTitleParts({sessionTitle: 'Sample session'}), [
    {key: 'session', label: 'Sample session'},
  ]);
  assert.deepEqual(
    paneTitleParts({sessionTitle: 'Sample session', profile: '', provider: '  ', model: undefined}),
    [{key: 'session', label: 'Sample session'}],
  );
  assert.equal(paneTitleText({sessionTitle: ' Sample session ', profile: 'default'}), 'Sample session · default');
});

test('a long part is cut at the configured length, keeping the head', () => {
  const limit = appConfig.pane_title_part_max_chars;
  assert.ok(limit > 0);
  const long = 'x'.repeat(limit + 20);
  assert.equal(truncatePaneTitlePart(long).length, limit + 1);
  assert.equal(truncatePaneTitlePart(long).endsWith('…'), true);
  assert.equal(truncatePaneTitlePart('short', limit), 'short');
  assert.equal(truncatePaneTitlePart(long, 0), long);
  const parts = paneTitleParts({sessionTitle: long, model: 'm'});
  assert.equal(parts[0].label, truncatePaneTitlePart(long));
  assert.equal(parts[1].label, 'm');
});
