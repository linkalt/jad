import assert from 'node:assert/strict';
import test from 'node:test';

import {isWebLink, parseSessionReadme, README_MISSING_MESSAGE} from './readme-view.ts';

test('a README answer is read as it is', () => {
  assert.deepEqual(parseSessionReadme({name: 'README.md', content: '# Hi', truncated: false}), {
    name: 'README.md',
    content: '# Hi',
    truncated: false,
  });
  assert.equal(parseSessionReadme({name: 'README.md', content: '', truncated: true}).truncated, true);
});

test('an answer of the wrong shape is refused', () => {
  for (const bad of [null, 'text', 3, {}, {name: 'a', content: 'b'}, {name: 1, content: 'b', truncated: false}, {name: 'a', content: 2, truncated: false}, {name: 'a', content: 'b', truncated: 'no'}]) {
    assert.throws(() => parseSessionReadme(bad), /invalid README data/, JSON.stringify(bad));
  }
});

test('only a web address counts as a link that leaves the app', () => {
  for (const href of ['https://example.com/a', 'http://example.com', 'HTTPS://EXAMPLE.COM']) {
    assert.equal(isWebLink(href), true, href);
  }
  for (const href of ['docs/x.md', '/docs/x.md', '#top', '../x', 'mailto:a@b.c', 'javascript:alert(1)', '//example.com', '']) {
    assert.equal(isWebLink(href), false, href);
  }
});

test('the missing-README message is the one the server sends', () => {
  assert.equal(README_MISSING_MESSAGE, 'no README file');
});
