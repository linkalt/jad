import assert from 'node:assert/strict';
import {test} from 'node:test';

import {cappedHeight} from './catalog-layout.ts';

test('the height is the pixel cap or the viewport share, whichever is smaller', () => {
  assert.equal(cappedHeight(420, 40), 'min(420px, 40dvh)');
  assert.equal(cappedHeight(380, 35.5), 'min(380px, 35.5dvh)');
});
