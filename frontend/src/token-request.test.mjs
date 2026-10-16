import assert from 'node:assert/strict';
import test from 'node:test';
import {registerTokenPresenter, requestApiToken} from './token-request.ts';

test('the registered presenter answers the request', async () => {
  const unregister = registerTokenPresenter(resolve => resolve('typed-secret'));
  try {
    assert.equal(await requestApiToken(), 'typed-secret');
  } finally {
    unregister();
  }
});

test('a dismissed presenter yields null', async () => {
  const unregister = registerTokenPresenter(resolve => resolve(null));
  try {
    assert.equal(await requestApiToken(), null);
  } finally {
    unregister();
  }
});

test('without a presenter the browser prompt is used, and null when there is none', async () => {
  const previous = globalThis.window;
  globalThis.window = {prompt: () => 'from-prompt'};
  try {
    assert.equal(await requestApiToken(), 'from-prompt');
    delete globalThis.window;
    assert.equal(await requestApiToken(), null);
  } finally {
    if (previous === undefined) delete globalThis.window;
    else globalThis.window = previous;
  }
});

test('an unregister of a replaced presenter keeps the newer one', async () => {
  const first = registerTokenPresenter(resolve => resolve('first'));
  const second = registerTokenPresenter(resolve => resolve('second'));
  first();
  try {
    assert.equal(await requestApiToken(), 'second');
  } finally {
    second();
  }
});
