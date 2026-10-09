import assert from 'node:assert/strict';
import {test} from 'node:test';

import {
  citationHost,
  citationLabel,
  safeCitationHref,
  hostedToolStatus,
} from './web-search-view.ts';

test('maps provider states of any hosted tool to row statuses', () => {
  for (const running of ['in_progress', 'searching', 'fetching', 'generating', 'interpreting', 'queued', 'running']) {
    assert.equal(hostedToolStatus(running), 'running', running);
  }
  for (const error of ['failed', 'incomplete', 'error', 'cancelled']) {
    assert.equal(hostedToolStatus(error), 'error', error);
  }
  // A state ending in "ing" is not running by that fact alone: a finished item
  // must never spin forever.
  for (const complete of ['completed', 'done', 'succeeded', undefined, '', 'something_new', 'pending', 'warning', 'missing', 'nothing']) {
    assert.equal(hostedToolStatus(complete), 'complete', String(complete));
  }
});

test('links only http and https sources', () => {
  assert.equal(safeCitationHref('https://go.dev/dl'), 'https://go.dev/dl');
  assert.equal(safeCitationHref('http://example.com/a?b=1'), 'http://example.com/a?b=1');
  assert.equal(safeCitationHref('javascript:alert(1)'), undefined);
  assert.equal(safeCitationHref('data:text/html,<b>x</b>'), undefined);
  assert.equal(safeCitationHref('file:///etc/passwd'), undefined);
  assert.equal(safeCitationHref('not a url'), undefined);
  assert.equal(safeCitationHref(''), undefined);
});

test('names a source by its title, else by its host', () => {
  assert.equal(citationHost('https://www.go.dev/dl'), 'go.dev');
  assert.equal(citationHost('https://docs.go.dev/x'), 'docs.go.dev');
  assert.equal(citationHost('not a url'), 'not a url');
  // These parse as URLs but have no host: the label must not come out empty.
  assert.equal(citationHost('javascript:alert(1)'), 'javascript:alert(1)');
  assert.equal(citationHost('mailto:a@b.test'), 'mailto:a@b.test');
  assert.equal(citationLabel({url: 'data:text/html,x'}), 'data:text/html,x');
  assert.equal(citationLabel({url: 'https://go.dev/dl', title: 'Downloads'}), 'Downloads');
  assert.equal(citationLabel({url: 'https://www.go.dev/dl', title: '  '}), 'go.dev');
  assert.equal(citationLabel({url: 'https://www.go.dev/dl'}), 'go.dev');
});
