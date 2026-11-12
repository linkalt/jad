import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

import {
  completedSubagentSteps,
  isSubagentToolName,
  subagentEarlierOmittedNote,
  subagentSectionOf,
  subagentTranscriptLabel,
  subagentTranscriptMessages,
  subagentWorkingNote,
} from './subagent-transcript.ts';
import {validSubagentTranscript} from './api.ts';

const text = value => [{type: 'text', text: value}];

test('no snapshot or no steps gives nothing', () => {
  assert.deepEqual(completedSubagentSteps(undefined), []);
  assert.deepEqual(completedSubagentSteps({steps: []}), []);
});

test('only completed steps with parts are kept, in order', () => {
  const steps = completedSubagentSteps({
    agent: 'researcher',
    steps: [
      {id: 'a', status: 'completed', parts: text('one')},
      {id: 'b', status: 'running', parts: text('two')},
      {id: 'c', status: 'pending', parts: text('three')},
      {id: 'd', status: 'completed', parts: []},
      {id: 'e', status: 'weird', parts: text('four')},
      {id: 'f', status: 'completed', parts: text('five')},
    ],
  });
  assert.deepEqual(steps.map(step => step.id), ['a', 'f']);
});

test('a malformed snapshot shows nothing instead of throwing', () => {
  assert.deepEqual(completedSubagentSteps({steps: 'no'}), []);
  assert.deepEqual(completedSubagentSteps({steps: [null]}), []);
  assert.deepEqual(completedSubagentSteps({agent: 3, steps: [{id: 'a', status: 'completed', parts: text('x')}]}), []);
  assert.deepEqual(completedSubagentSteps({steps: [{id: 'a', status: 'completed', parts: [{type: 'text'}]}]}), []);
});

const reasoning = value => ({type: 'reasoning', text: value});
const call = callId => ({type: 'tool_call', callId, name: 'read', arguments: '{}'});
const result = callId => ({type: 'tool_result', callId, output: 'ok', status: 'success'});
const step = (id, parts) => ({id, status: 'completed', parts});
const shown = (steps, result) => subagentTranscriptMessages({steps}, result).flatMap(message => message.parts);
// What the tool result says of the child's last text (the server sets the flag from the
// result's own information, webui.SubagentCallOf): a finished, stopped or timed-out result
// holds it whole and sends no flag (an older server sends none either, and reads the same);
// a handed-off, cut, failed or lost result does not hold it whole and says so.
const finished = {};
const omitsLastText = {omitsLastText: true};

test('finished: with a tool call, the trailing text-only step (the answer the result shows) is dropped whole', () => {
  const [message] = subagentTranscriptMessages({
    steps: [step('1', [call('a'), result('a')]), step('2', text('final summary'))],
  });
  assert.deepEqual(message.parts, [call('a'), result('a')]);
  assert.deepEqual(message.steps, [{id: '1', startPart: 0, endPart: 2, hasTools: true}]);
});

test('finished: with a tool call, the trailing step keeps its reasoning and loses only its text', () => {
  const [message] = subagentTranscriptMessages({
    steps: [step('1', [call('a'), result('a')]), step('2', [reasoning('wrap up'), ...text('final summary')])],
  });
  assert.deepEqual(message.parts, [call('a'), result('a'), reasoning('wrap up')]);
  assert.deepEqual(message.steps.map(s => [s.id, s.startPart, s.endPart, s.hasTools]), [
    ['1', 0, 2, true],
    ['2', 2, 3, false],
  ]);
});

test('finished: only the trailing step loses its text; earlier text stays', () => {
  assert.deepEqual(
    shown([step('1', [...text('plan'), call('a'), result('a')]), step('2', text('summary'))]),
    [...text('plan'), call('a'), result('a')],
  );
});

test('finished: a snapshot with a single tool step and its text keeps the calls, drops the text', () => {
  assert.deepEqual(shown([step('1', [call('a'), result('a'), ...text('done')])]), [call('a'), result('a')]);
  assert.deepEqual(shown([step('1', [call('a'), result('a'), ...text('done')])], {omitsLastText: false}), [call('a'), result('a')]);
});

test('still running: a handed-off child keeps the text of its last completed step, which is not its end', () => {
  // The child is between steps: its last completed step said what it does next and the
  // result above holds only the run id, so that text is nowhere else.
  const steps = [step('1', [...text('reading the config'), call('a'), result('a')]), step('2', [...text('now the tests'), call('b'), result('b')])];
  assert.deepEqual(shown(steps, omitsLastText), [...text('reading the config'), call('a'), result('a'), ...text('now the tests'), call('b'), result('b')]);
  assert.deepEqual(shown(steps, finished), [...text('reading the config'), call('a'), result('a'), call('b'), result('b')]);
});

test('handed off: a child that ended after its result was served keeps its answer, which the result never held', () => {
  const steps = [step('1', [call('a'), result('a')]), step('2', [reasoning('wrap up'), ...text('final summary')])];
  const [message] = subagentTranscriptMessages({steps}, omitsLastText);
  assert.deepEqual(message.parts, [call('a'), result('a'), reasoning('wrap up'), ...text('final summary')]);
  assert.deepEqual(message.steps.map(s => [s.id, s.startPart, s.endPart, s.hasTools]), [
    ['1', 0, 2, true],
    ['2', 2, 4, false],
  ]);
  // The same snapshot under a result that came back with the child's end loses that text.
  assert.deepEqual(shown(steps, finished), [call('a'), result('a'), reasoning('wrap up')]);
});

test('finished and cut: a result that holds only the head of the answer leaves the whole answer in the transcript', () => {
  // The result names truncated: it shows the first bytes of the answer, the transcript holds it whole.
  const steps = [step('1', [call('a'), result('a')]), step('2', text('the whole answer, past the cut'))];
  assert.deepEqual(shown(steps, omitsLastText), [call('a'), result('a'), ...text('the whole answer, past the cut')]);
  assert.deepEqual(shown(steps, finished), [call('a'), result('a')]);
});

test('failed: a result that holds no text of the child leaves its last text in the transcript', () => {
  // A failed result carries only the reason; what the child last wrote is only in the transcript.
  const steps = [step('1', [...text('trying the build'), call('a'), result('a')])];
  assert.deepEqual(shown(steps, omitsLastText), [...text('trying the build'), call('a'), result('a')]);
  assert.deepEqual(shown(steps, finished), [call('a'), result('a')]);
});

test('lost: a result whose text can be a step the snapshot withholds leaves the shown last text in place', () => {
  // The server died while step 3 ran its tool: the result holds step 3's text, the snapshot
  // ends at step 2, whose text is nowhere else.
  const steps = [step('1', [call('a'), result('a')]), step('2', [...text('step two notes'), call('b'), result('b')])];
  assert.deepEqual(shown(steps, omitsLastText), [call('a'), result('a'), ...text('step two notes'), call('b'), result('b')]);
});

test('stopped and timed out: a result that holds the child\'s last text whole reads like a finished one', () => {
  // Their content is the newest text of a committed step (app/subagent.go:128), and the
  // runner writes a result for every call a cancelled round leaves (session/runner.go:1121),
  // so that step closes and is the snapshot's last one with text.
  const steps = [step('1', [call('a'), result('a')]), step('2', [...text('half done'), call('b'), result('b')])];
  assert.deepEqual(shown(steps, finished), [call('a'), result('a'), call('b'), result('b')]);
});

test('without any tool call a finished child ends with Done in place of its summary, reasoning kept', () => {
  const done = text('Done');
  assert.deepEqual(shown([step('1', [reasoning('think')]), step('2', text('answer'))], finished), [reasoning('think'), ...done]);
  assert.deepEqual(shown([step('1', text('answer'))], finished), done);
  // Reasoning and summary in one step: the reasoning stays, the summary becomes the word.
  assert.deepEqual(shown([step('1', [reasoning('think'), ...text('answer')])], finished), [reasoning('think'), ...done]);
  // Several text parts of the last step are one finish; an earlier step's text is the child's own and stays.
  assert.deepEqual(
    shown([step('1', text('first thoughts')), step('2', [...text('a'), reasoning('r'), ...text('b')])], finished),
    [...text('first thoughts'), ...done, reasoning('r')],
  );
  // The step range still covers the step, so the renderer groups it as before.
  const [message] = subagentTranscriptMessages({steps: [step('1', [reasoning('think'), ...text('answer')])]}, finished);
  assert.deepEqual(message.steps, [{id: '1', startPart: 0, endPart: 2, hasTools: false}]);
  // The word is not the tool result: the result's text never reaches the transcript.
  assert.ok(!JSON.stringify(message).includes('answer'));
});

test('a last text that carries images or files is the child\'s content and is kept', () => {
  for (const extra of [{images: ['a.png']}, {files: ['b.pdf']}]) {
    const parts = [{type: 'text', text: 'answer', ...extra}];
    assert.deepEqual(shown([step('1', parts)], finished), parts);
  }
});

test('a child without a tool call keeps its summary whenever the result does not hold it whole or the child is live', () => {
  const steps = [step('1', [reasoning('think')]), step('2', text('answer'))];
  const whole = [reasoning('think'), ...text('answer')];
  assert.deepEqual(shown(steps, omitsLastText), whole);
  assert.deepEqual(shown(steps, {unsettled: true}), whole);
  assert.deepEqual(shownWith(steps, true, finished), whole);
  assert.deepEqual(subagentTranscriptMessages({steps, running: true}, finished).flatMap(message => message.parts), whole);
});

test('a finished child with a tool call gets no Done: the summary is dropped as before', () => {
  const steps = [step('1', [reasoning('r'), call('a'), result('a')]), step('2', text('final'))];
  assert.deepEqual(shown(steps, finished), [reasoning('r'), call('a'), result('a')]);
});

test('nothing surviving gives no message, so no button renders', () => {
  assert.deepEqual(subagentTranscriptMessages(undefined), []);
  assert.deepEqual(subagentTranscriptMessages({steps: [{id: '1', status: 'running', parts: [call('a')]}]}), []);
  assert.deepEqual(subagentTranscriptMessages({steps: 'no'}), []);
  // Running steps do not count as tool calls: the completed text-only snapshot stays whole.
  assert.deepEqual(shown([step('1', text('x')), {id: '2', status: 'running', parts: [call('a')]}]), text('Done'));
});

// The snapshot's own bit (webui.SubagentSnapshot.MayHaveNewer): its last step may not be the
// child's newest response, so the result's text may belong to a step it does not show.
const shownWith = (steps, mayHaveNewer, result) =>
  subagentTranscriptMessages(mayHaveNewer === undefined ? {steps} : {steps, mayHaveNewer}, result).flatMap(message => message.parts);

test('a snapshot that may have a newer response keeps its last step\'s text, whatever the result holds', () => {
  // The snapshot stopped at its step limit (or byte budget, or withheld a step): the result's
  // text is a later step's, so step 2's text is shown nowhere else.
  const steps = [step('1', [call('a'), result('a')]), step('2', [...text('step two notes'), call('b'), result('b')])];
  const all = [call('a'), result('a'), ...text('step two notes'), call('b'), result('b')];
  assert.deepEqual(shownWith(steps, true, finished), all);
  assert.deepEqual(shownWith(steps, true, omitsLastText), all);
  const textOnly = [step('1', [call('a'), result('a')]), step('2', text('last shown'))];
  assert.deepEqual(shownWith(textOnly, true, finished), [call('a'), result('a'), ...text('last shown')]);
});

test('a snapshot that does not say so keeps the old rule, bit absent or false', () => {
  const steps = [step('1', [call('a'), result('a')]), step('2', [...text('final'), reasoning('r')])];
  for (const bit of [undefined, false]) {
    assert.deepEqual(shownWith(steps, bit, finished), [call('a'), result('a'), reasoning('r')], String(bit));
    assert.deepEqual(shownWith(steps, bit, omitsLastText), [call('a'), result('a'), ...text('final'), reasoning('r')], String(bit));
  }
  // A malformed bit that reaches the mapper unchecked keeps text rather than hiding it.
  assert.deepEqual(shownWith(steps, 'yes', finished), [call('a'), result('a'), ...text('final'), reasoning('r')]);
});

// Live: nothing is left out while the parent's call is unsettled or the child still runs.
const live = (steps, extra, result) => subagentTranscriptMessages({steps, ...extra}, result).flatMap(message => message.parts);

test('no trim while the call is unsettled or the child is running; the trim is unchanged for a settled, finished, uncut result', () => {
  const steps = [step('1', [call('a'), result('a')]), step('2', [...text('wrap up'), call('b'), result('b')]), step('3', text('final summary'))];
  const whole = [call('a'), result('a'), ...text('wrap up'), call('b'), result('b'), ...text('final summary')];
  // Unsettled (no result above holds the text): everything shows, even with omitsLastText absent.
  assert.deepEqual(live(steps, {}, {unsettled: true}), whole);
  // Running (the child's last step is not its end): everything shows, though the result is settled and uncut.
  assert.deepEqual(live(steps, {running: true}, finished), whole);
  assert.deepEqual(live(steps, {running: true}, {unsettled: true}), whole);
  // Settled, uncut and finished: today's trim.
  // (only the trailing text-only step goes; earlier text stays.)
  const trimmed = [call('a'), result('a'), ...text('wrap up'), call('b'), result('b')];
  assert.deepEqual(live(steps, {}, {unsettled: false}), trimmed);
  assert.deepEqual(live(steps, {}, finished), trimmed);
  assert.deepEqual(live(steps, {running: false}, {omitsLastText: false, unsettled: false}), trimmed);
});

test('a running child with no completed step shows no message, and a zero-step answer never throws', () => {
  assert.deepEqual(subagentTranscriptMessages({steps: [], running: true}, {unsettled: true}), []);
  assert.deepEqual(subagentTranscriptMessages({steps: [], running: true}, omitsLastText), []);
});

test('the section: a subagent call with no result yet has one; a settled result has one only with a summary', () => {
  for (const name of ['subagent', 'native.subagent']) {
    assert.deepEqual(subagentSectionOf({name}, undefined), {settled: false});
    assert.equal(isSubagentToolName(name), true);
  }
  assert.equal(subagentSectionOf({name: 'read'}, undefined), undefined, 'another tool has no section while it runs');
  assert.equal(isSubagentToolName('profile_message'), false);
  const summary = {steps: 2, omitsLastText: true};
  assert.deepEqual(subagentSectionOf({name: 'subagent'}, {subagent: summary}), {summary, settled: true});
  assert.deepEqual(subagentSectionOf({name: 'subagent'}, {subagent: {steps: 0, omitsLastText: true}}), {summary: {steps: 0, omitsLastText: true}, settled: true});
  // A settled result without a summary (a child that failed before its first step) has none:
  // a section shown while the call ran goes away then, as it always did for a settled call.
  assert.equal(subagentSectionOf({name: 'subagent'}, {}), undefined);
  assert.equal(subagentSectionOf({name: 'read'}, {subagent: undefined}), undefined);
});

test('the label: running with no step completed yet, running with a count, or the settled count', () => {
  const label = input => subagentTranscriptLabel(input);
  // Unsettled, nothing read yet (closed, loading or starting).
  assert.equal(label({isOpen: false}), 'Show Transcript · running, no step completed yet');
  assert.equal(label({isOpen: true}), 'Hide Transcript · running, no step completed yet');
  // A handed-off child with a summary of no step is running until the panel says otherwise.
  assert.equal(label({isOpen: false, summary: {steps: 0, omitsLastText: true}}), 'Show Transcript · running, no step completed yet');
  // A summary with steps is the count, as before.
  assert.equal(label({isOpen: false, summary: {steps: 1}}), 'Show Transcript · 1 step');
  assert.equal(label({isOpen: true, summary: {steps: 7}}), 'Hide Transcript · 7 steps');
  // Open and read: the transcript's own count and running bit.
  assert.equal(label({isOpen: true, transcript: {steps: [], running: true}}), 'Hide Transcript · running, no step completed yet');
  assert.equal(label({isOpen: true, transcript: {steps: [step('1', text('a'))], running: true}}), 'Hide Transcript · running, 1 step');
  assert.equal(label({isOpen: true, summary: {steps: 0}, transcript: {steps: [step('1', text('a')), step('2', text('b'))], running: true}}), 'Hide Transcript · running, 2 steps');
  assert.equal(label({isOpen: true, summary: {steps: 2}, transcript: {steps: [step('1', text('a')), step('2', text('b'))]}}), 'Hide Transcript · 2 steps');
  // A handed-off child that ended with no step says so once it has been read.
  assert.equal(label({isOpen: true, summary: {steps: 0}, transcript: {steps: []}}), 'Hide Transcript · 0 steps');
});

test('a snapshot that omits earlier steps says so, in any form the server might send', () => {
  const note = 'Earlier steps are not shown.';
  const steps = [step('9', text('late'))];
  assert.equal(subagentEarlierOmittedNote({steps, earlierOmitted: true}), note);
  // Anything but an absent field or an explicit false counts as set, as for mayHaveNewer.
  assert.equal(subagentEarlierOmittedNote(validSubagentTranscript({steps, earlierOmitted: 'yes'})), note);
  assert.equal(subagentEarlierOmittedNote(validSubagentTranscript({steps, earlierOmitted: true})), note);
  assert.equal(subagentEarlierOmittedNote(validSubagentTranscript({steps, earlierOmitted: false})), undefined);
  assert.equal(subagentEarlierOmittedNote(validSubagentTranscript({steps})), undefined);
  assert.equal(subagentEarlierOmittedNote(undefined), undefined);
  // The server's body for a window with no line start: no step, the flag set.
  const empty = validSubagentTranscript(JSON.parse('{"steps":[],"earlierOmitted":true}'));
  assert.equal(subagentEarlierOmittedNote(empty), note);
  assert.deepEqual(subagentTranscriptMessages(empty), []);
});

test('the working line does not number a step when earlier steps were left out', () => {
  const steps = [step('1.9', text('x'))];
  assert.equal(subagentWorkingNote({steps, running: true}), 'Working on step 2…');
  assert.equal(subagentWorkingNote({steps, running: true, earlierOmitted: true}), 'Working on the next step…');
});

// The panel itself is not rendered here (it needs the poller and the whole transcript
// renderer), so its wiring is pinned in the source: the note comes from the snapshot, sits
// above the transcript, and the running line is the one that knows about omitted steps.
test('the panel shows the omission note above the transcript and uses the working line', () => {
  const source = readFileSync(new URL('./components/SubagentTranscript.tsx', import.meta.url), 'utf8');
  assert.match(source, /const earlierNote = subagentEarlierOmittedNote\(transcript\);/);
  const note = source.indexOf('{earlierNote !== undefined && (');
  const empty = source.indexOf('No steps to show.');
  const body = source.indexOf('<ReadOnlyTranscript messages={messages} />');
  assert.ok(note > 0 && note < empty && empty < body, 'the note comes before the empty line and the transcript');
  assert.match(source, /\{subagentWorkingNote\(transcript\)\}/);
  assert.doesNotMatch(source, /Working on step \{/);
});
