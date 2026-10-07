import assert from 'node:assert/strict';
import test from 'node:test';

import {interactionEndNotice, parseInteractionEnd} from './interaction-end.ts';

const wire = {interaction_id: 'i1', kind: 'approval', tool: 'bash', outcome: 'answered', decision: 'accept'};
const elsewhere = {answeredHere: false, stoppedHere: false};

test('reads the wire form of an end', () => {
  assert.deepEqual(parseInteractionEnd(wire), {
    interactionId: 'i1',
    kind: 'approval',
    tool: 'bash',
    outcome: 'answered',
    decision: 'accept',
  });
  assert.deepEqual(parseInteractionEnd({interaction_id: 'i1', kind: 'draft', tool: 'request_draft', outcome: 'timed_out'}), {
    interactionId: 'i1',
    kind: 'draft',
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
    {...wire, kind: 'other'},
    {...wire, tool: 4},
    {...wire, outcome: 'done'},
    {...wire, decision: 'maybe'},
    {...wire, decision: undefined},
    {interaction_id: 'i1', kind: 'approval', tool: 'bash', outcome: 'timed_out', decision: 'accept'},
  ]) {
    assert.equal(parseInteractionEnd(bad), undefined, JSON.stringify(bad));
  }
});

test('the tab that answered stays quiet, any other tab is told who decided', () => {
  const allowed = parseInteractionEnd(wire);
  assert.equal(interactionEndNotice(allowed, {answeredHere: true, stoppedHere: false}), undefined);
  assert.deepEqual(interactionEndNotice(allowed, elsewhere), {
    title: 'Answered in another window',
    message: 'Another window allowed bash.',
  });
  assert.deepEqual(interactionEndNotice(parseInteractionEnd({...wire, decision: 'decline'}), elsewhere), {
    title: 'Answered in another window',
    message: 'Another window denied bash, so it did not run.',
  });
});

test('a draft is worded as a draft, with accept, edit and decline told apart', () => {
  const draft = {interaction_id: 'i2', kind: 'draft', tool: 'request_draft', outcome: 'answered'};
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

test('a timeout is told to every tab, and says the call did not run', () => {
  const approval = parseInteractionEnd({interaction_id: 'i1', kind: 'approval', tool: 'bash', outcome: 'timed_out'});
  const expected = {title: 'The approval timed out', message: 'Nobody answered in time, so bash did not run.'};
  assert.deepEqual(interactionEndNotice(approval, elsewhere), expected);
  assert.deepEqual(interactionEndNotice(approval, {answeredHere: true, stoppedHere: true}), expected);
  assert.deepEqual(
    interactionEndNotice(parseInteractionEnd({interaction_id: 'i2', kind: 'draft', tool: 'request_draft', outcome: 'timed_out'}), elsewhere),
    {title: 'The draft timed out', message: 'Nobody answered in time; the draft for request_draft was not confirmed.'},
  );
});

test('a stop is told to a tab that did not ask for it, and not to the one that did', () => {
  const stopped = parseInteractionEnd({interaction_id: 'i1', kind: 'approval', tool: 'bash', outcome: 'cancelled'});
  assert.deepEqual(interactionEndNotice(stopped, elsewhere), {
    title: 'The run stopped',
    message: 'The run stopped before the approval for bash was answered.',
  });
  assert.equal(interactionEndNotice(stopped, {answeredHere: false, stoppedHere: true}), undefined);
});
