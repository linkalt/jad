import assert from 'node:assert/strict';
import test from 'node:test';

import {interactionEndNotice, parseInteractionEnd} from './interaction-end.ts';

const wire = {interaction_id: 'i1', tool: 'request_draft', outcome: 'answered', decision: 'accept'};
const elsewhere = {answeredHere: false, stoppedHere: false};

test('reads the wire form of an end', () => {
  assert.deepEqual(parseInteractionEnd(wire), {
    interactionId: 'i1',
    tool: 'request_draft',
    outcome: 'answered',
    decision: 'accept',
  });
  assert.deepEqual(parseInteractionEnd({interaction_id: 'i1', tool: 'request_draft', outcome: 'timed_out'}), {
    interactionId: 'i1',
    tool: 'request_draft',
    outcome: 'timed_out',
  });
});

test('a missing or malformed end reads as nothing, so it can never become a wrong notice', () => {
  for (const bad of [
    undefined,
    null,
    'x',
    [],
    {...wire, interaction_id: ''},
    {...wire, interaction_id: 4},
    {...wire, tool: 4},
    {...wire, outcome: 'done'},
    {...wire, decision: 'maybe'},
    {...wire, decision: undefined},
    {interaction_id: 'i1', tool: 'request_draft', outcome: 'timed_out', decision: 'accept'},
  ]) {
    assert.equal(parseInteractionEnd(bad), undefined, JSON.stringify(bad));
  }
});

test('the tab that answered stays quiet, any other tab is told who decided', () => {
  const accepted = parseInteractionEnd(wire);
  assert.equal(interactionEndNotice(accepted, {answeredHere: true, stoppedHere: false}), undefined);
  assert.deepEqual(interactionEndNotice(accepted, elsewhere), {
    title: 'Answered in another window',
    message: 'Another window accepted the draft for request_draft.',
  });
});

test('a draft is worded as a draft, with accept, edit and decline told apart', () => {
  const draft = {interaction_id: 'i2', tool: 'request_draft', outcome: 'answered'};
  assert.equal(
    interactionEndNotice(parseInteractionEnd({...draft, decision: 'accept'}), elsewhere).message,
    'Another window accepted the draft for request_draft.',
  );
  assert.equal(
    interactionEndNotice(parseInteractionEnd({...draft, decision: 'update'}), elsewhere).message,
    'Another window accepted an edited draft for request_draft.',
  );
  assert.equal(
    interactionEndNotice(parseInteractionEnd({...draft, decision: 'decline'}), elsewhere).message,
    'Another window declined the draft for request_draft.',
  );
});

test('a timeout is told to every tab', () => {
  const timedOut = parseInteractionEnd({interaction_id: 'i2', tool: 'request_draft', outcome: 'timed_out'});
  const expected = {title: 'The draft timed out', message: 'Nobody answered in time; the draft for request_draft was not confirmed.'};
  assert.deepEqual(interactionEndNotice(timedOut, elsewhere), expected);
  assert.deepEqual(interactionEndNotice(timedOut, {answeredHere: true, stoppedHere: true}), expected);
});

test('a stop is told to a tab that did not ask for it, and not to the one that did', () => {
  const stopped = parseInteractionEnd({interaction_id: 'i1', tool: 'request_draft', outcome: 'cancelled'});
  assert.deepEqual(interactionEndNotice(stopped, elsewhere), {
    title: 'The run stopped',
    message: 'The run stopped before the draft for request_draft was answered.',
  });
  assert.equal(interactionEndNotice(stopped, {answeredHere: false, stoppedHere: true}), undefined);
});
