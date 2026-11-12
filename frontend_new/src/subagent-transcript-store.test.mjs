import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
const transcript = {steps: [{id: '1.1', status: 'completed', parts: [{type: 'text', text: 'child'}]}]};
let reply = () => new Response(JSON.stringify({subagent: transcript}), {status: 200});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};

const {fetchSubagentTranscript, validSubagentSummary} = await import('./api.ts');
const {createSubagentTranscriptStore, subagentTranscriptKey} = await import('./subagent-transcript-store.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

const url = request => new URL(request.url, 'http://localhost');

test('the summary is the agent, a count of zero or more steps and whether the result omits the child\'s last text', () => {
  assert.deepEqual(validSubagentSummary({agent: 'explore', steps: 7}), {agent: 'explore', steps: 7});
  assert.deepEqual(validSubagentSummary({steps: 1}), {steps: 1});
  // A child handed off before its first step has a summary of no step.
  assert.deepEqual(validSubagentSummary({steps: 0, omitsLastText: true}), {steps: 0, omitsLastText: true});
  assert.deepEqual(validSubagentSummary({agent: 'explore', steps: 0}), {agent: 'explore', steps: 0});
  assert.deepEqual(validSubagentSummary({steps: 2, omitsLastText: true}), {steps: 2, omitsLastText: true});
  for (const bad of [undefined, null, 'x', {}, {steps: -1}, {steps: 1.5}, {steps: '7'}, {agent: 3, steps: 2}, {steps: [{id: 'a', status: 'completed', parts: []}]}, {steps: 2, omitsLastText: 'yes'}]) {
    assert.equal(validSubagentSummary(bad), undefined, JSON.stringify(bad));
  }
});

test('the transcript is fetched by session, call id and profile, and nothing before it is asked for', async () => {
  requests.length = 0;
  assert.equal(requests.length, 0, 'importing the client or the store makes no request');
  const result = await fetchSubagentTranscript('my session', 'toolu/01', 'jad/test');
  assert.deepEqual(result, transcript);
  assert.equal(requests.length, 1);
  assert.equal(url(requests[0]).pathname, '/api/sessions/my%20session/subagents/toolu%2F01');
  assert.equal(url(requests[0]).searchParams.get('profile'), 'jad/test');
  await fetchSubagentTranscript('s', 'c');
  assert.equal(url(requests[1]).search, '');
});

test('the bit that the last step may not be the newest response survives validation, and absent stays absent', async () => {
  const steps = transcript.steps;
  reply = () => new Response(JSON.stringify({subagent: {steps, mayHaveNewer: true}}), {status: 200});
  assert.deepEqual(await fetchSubagentTranscript('s', 'c'), {steps, mayHaveNewer: true});
  // The server omits it when false; an explicit false is read as absent, a stray value as set.
  reply = () => new Response(JSON.stringify({subagent: {agent: 'a', steps, mayHaveNewer: false}}), {status: 200});
  assert.deepEqual(await fetchSubagentTranscript('s', 'c'), {agent: 'a', steps});
  reply = () => new Response(JSON.stringify({subagent: {steps, mayHaveNewer: 'yes'}}), {status: 200});
  assert.deepEqual(await fetchSubagentTranscript('s', 'c'), {steps, mayHaveNewer: true});
  reply = () => new Response(JSON.stringify({subagent: transcript}), {status: 200});
  assert.deepEqual(await fetchSubagentTranscript('s', 'c'), transcript);
});

test('running survives validation only as an explicit true; absent means finished', async () => {
  const steps = transcript.steps;
  reply = () => new Response(JSON.stringify({subagent: {steps, running: true}}), {status: 200});
  assert.deepEqual(await fetchSubagentTranscript('s', 'c'), {steps, running: true});
  reply = () => new Response(JSON.stringify({subagent: {agent: 'a', steps, mayHaveNewer: true, running: true}}), {status: 200});
  assert.deepEqual(await fetchSubagentTranscript('s', 'c'), {agent: 'a', steps, mayHaveNewer: true, running: true});
  // A finished child (no key, as an older server too), or a stray value, does not keep the panel polling.
  for (const running of [false, 'yes', 1, null]) {
    reply = () => new Response(JSON.stringify({subagent: {steps, running}}), {status: 200});
    assert.deepEqual(await fetchSubagentTranscript('s', 'c'), {steps}, JSON.stringify(running));
  }
  reply = () => new Response(JSON.stringify({subagent: transcript}), {status: 200});
  assert.deepEqual(await fetchSubagentTranscript('s', 'c'), transcript);
});

test('a server error and a malformed body are errors, not a broken transcript', async () => {
  reply = () => new Response(JSON.stringify({error: 'subagent transcript not found'}), {status: 404});
  await assert.rejects(() => fetchSubagentTranscript('s', 'unknown'), error => error.status === 404 && /subagent transcript not found/.test(error.message));
  for (const body of [{}, {subagent: {steps: 'no'}}, {subagent: {steps: [null]}}, {subagent: {agent: 1, steps: []}}, []]) {
    reply = () => new Response(JSON.stringify(body), {status: 200});
    await assert.rejects(() => fetchSubagentTranscript('s', 'c'), /invalid subagent transcript/, JSON.stringify(body));
  }
  reply = () => new Response(JSON.stringify({subagent: transcript}), {status: 200});
});

test('a result that printed nothing and one malformed part do not take the transcript down', async () => {
  // Go omits an empty output (`omitempty`), so a call whose tool printed nothing has a
  // result without `output`. A part the client cannot read is dropped; its step stays.
  const call = {type: 'tool_call', callId: 'k1', name: 'bash', arguments: '{}'};
  reply = () =>
    new Response(
      JSON.stringify({
        subagent: {
          steps: [
            {id: '1.1', status: 'completed', parts: [call, {type: 'tool_result', callId: 'k1', status: 'success'}]},
            {id: '1.2', status: 'completed', parts: [{type: 'text'}, {type: 'text', text: 'kept'}]},
          ],
        },
      }),
      {status: 200},
    );
  const answer = await fetchSubagentTranscript('s', 'c');
  assert.deepEqual(answer.steps.map(step => step.parts), [
    [call, {type: 'tool_result', callId: 'k1', status: 'success', output: ''}],
    [{type: 'text', text: 'kept'}],
  ]);
  reply = () => new Response(JSON.stringify({subagent: transcript}), {status: 200});
});

test('a transcript with no step yet is a valid answer', async () => {
  reply = () => new Response(JSON.stringify({subagent: {steps: []}}), {status: 200});
  assert.deepEqual(await fetchSubagentTranscript('s', 'c'), {steps: []});
  reply = () => new Response(JSON.stringify({subagent: transcript}), {status: 200});
});

test('the store fetches nothing until asked, every refresh asks the server, and the newest answer replaces the entry', async () => {
  let fetches = 0;
  const answers = [transcript, {steps: [...transcript.steps, {id: '1.2', status: 'completed', parts: []}], running: true}];
  const fetch = async () => answers[fetches++];
  const store = createSubagentTranscriptStore(4);
  const key = subagentTranscriptKey('s', 'p', 'c1');
  assert.equal(store.peek(key), undefined);
  assert.equal(fetches, 0, 'peek never fetches');
  assert.equal(await store.refresh(key, fetch), transcript);
  assert.equal(store.peek(key), transcript);
  assert.equal(await store.refresh(key, fetch), answers[1]);
  assert.equal(fetches, 2, 'a second refresh asks again: a running child has more by now');
  assert.equal(store.peek(key), answers[1], 'and replaces the entry under the same key');
  // Another call is another entry, and only when it is asked for.
  await store.refresh(subagentTranscriptKey('s', 'p', 'c2'), fetch);
  assert.equal(store.peek(key), answers[1]);
});

test('callers that ask while a request is in flight share it; a failure keeps the entry and is retried', async () => {
  const store = createSubagentTranscriptStore(4);
  let fetches = 0;
  let settle;
  const slow = () => {
    fetches += 1;
    return new Promise((resolve, reject) => {
      settle = {resolve, reject};
    });
  };
  const first = store.refresh('k', slow);
  const second = store.refresh('k', slow);
  assert.equal(fetches, 1);
  settle.reject(new Error('boom'));
  await assert.rejects(first, /boom/);
  await assert.rejects(second, /boom/);
  assert.equal(store.peek('k'), undefined, 'a failure is not cached');
  const retry = store.refresh('k', slow);
  assert.equal(fetches, 2, 'retry asks again');
  settle.resolve(transcript);
  assert.equal(await retry, transcript);
  const later = store.refresh('k', slow);
  settle.reject(new Error('later boom'));
  await assert.rejects(later, /later boom/);
  assert.equal(store.peek('k'), transcript, 'a failed refresh leaves the last good answer');
});

test('the store keeps a bounded number of transcripts, dropping the least recently used', async () => {
  const store = createSubagentTranscriptStore(2);
  const make = id => async () => ({steps: [{id, status: 'completed', parts: []}]});
  await store.refresh('a', make('a'));
  await store.refresh('b', make('b'));
  store.peek('a');
  await store.refresh('c', make('c'));
  assert.equal(store.peek('b'), undefined);
  assert.ok(store.peek('a') && store.peek('c'));
});

test('the key is session, profile and call id, without the summary\'s frozen step count', () => {
  assert.equal(subagentTranscriptKey('s', 'p', 'c'), subagentTranscriptKey('s', 'p', 'c'));
  assert.equal(subagentTranscriptKey.length, 3, 'no step count parameter');
  assert.notEqual(subagentTranscriptKey('s', 'p', 'c'), subagentTranscriptKey('s', undefined, 'c'));
  assert.notEqual(subagentTranscriptKey('s', 'p', 'c'), subagentTranscriptKey('t', 'p', 'c'));
  assert.notEqual(subagentTranscriptKey('s', 'p', 'c'), subagentTranscriptKey('s', 'p', 'd'));
});

// No DOM in this suite, so what a closed transcript can run is pinned at the source.
const component = name => readFileSync(new URL(`./components/${name}`, import.meta.url), 'utf8');
const code = source => source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');

test('closed, the section is one button: no request, no mapping, no timer and no transcript render before it is open', () => {
  const view = code(component('SubagentTranscript.tsx'));
  const closed = view.slice(view.indexOf('export function SubagentTranscript('), view.indexOf('function OpenSubagentTranscript'));
  assert.doesNotMatch(closed, /fetchSubagentTranscript|subagentTranscriptMessages|<ReadOnlyTranscript|transcriptStore|validSubagentTranscript|setInterval|setTimeout/);
  assert.match(closed, /useState\(false\)/);
  // History is not validated part by part, so the section checks the summary itself: a
  // malformed one (null, a negative or fractional count) renders nothing instead of throwing.
  assert.match(closed, /summary === undefined \|\| validSubagentSummary\(summary\) !== undefined/);
  assert.match(closed, /if \(!isShown\) \{\s*return null;/);
  // The poll runs only while the panel is open; the label is the shared pure function.
  assert.match(closed, /enabled: isOpen && isShown/);
  assert.match(closed, /subagentTranscriptLabel\(\{\s*isOpen,\s*summary,/);
  assert.match(closed, /onClick=\{\(\) => setIsOpen\(open => !open\)\}/);
  // Everything that costs something lives in the open component, mounted only while open.
  assert.equal((closed.match(/<OpenSubagentTranscript/g) ?? []).length, 1);
  assert.match(closed, /\{isOpen && \(\s*<OpenSubagentTranscript/);
  const open = view.slice(view.indexOf('function OpenSubagentTranscript'));
  // The tool result says whether it omits the child's last text, and the call whether it has settled.
  assert.match(open, /subagentTranscriptMessages\(transcript, \{omitsLastText, unsettled: !settled\}\)/);
  assert.match(closed, /omitsLastText=\{summary\?\.omitsLastText === true\}/);
  assert.equal((open.match(/<ReadOnlyTranscript/g) ?? []).length, 1);
  // A running child ends with the step it works on; a failure has a retry; a start is shown.
  assert.match(open, /\{subagentWorkingNote\(transcript\)\}/);
  assert.match(open, /Retry/);
  assert.match(open, /Starting…/);
});

test('the poll belongs to the open panel: one poller per open call, stopped on close, cadence from config', () => {
  const view = code(component('SubagentTranscript.tsx'));
  const hook = view.slice(view.indexOf('function useSubagentPoll'), view.indexOf('export function SubagentTranscript('));
  // The effect that owns the poller depends on what identifies the call, so the host's
  // re-render with a fresh message does not restart it; the call's state reaches it by update.
  assert.match(hook, /if \(!enabled \|\| sessionRef === undefined\) return undefined;/);
  assert.match(hook, /\[enabled, sessionRef, profile, callId\]/);
  assert.match(hook, /poller\.stop\(\);/);
  assert.match(hook, /pollerRef\.current\?\.update\(\{settled, summarySteps\}\)/);
  assert.match(hook, /pollMs: appConfig\.subagent_transcript_poll_ms/);
  assert.match(hook, /maxDelayMs: appConfig\.subagent_transcript_poll_max_delay_ms/);
  assert.match(hook, /fetchSubagentTranscript\(sessionRef, callId, profile\)/);
  assert.match(view, /document\.visibilityState !== 'hidden'/);
  assert.match(view, /addEventListener\('visibilitychange'/);
  // The component itself owns no timer: the poller does, and stops them.
  assert.doesNotMatch(view, /setInterval|setTimeout|prefetch/);
  const poller = code(readFileSync(new URL('./subagent-transcript-poll.ts', import.meta.url), 'utf8'));
  assert.doesNotMatch(poller, /setInterval/);
  assert.match(poller, /clearTimeout\(timer\)/);
});

test('the panel shows the section for a subagent call that has no result yet, keyed by the open call', () => {
  const panel = code(component('ActivityPanel.tsx'));
  assert.match(panel, /subagentSectionOf\(detail\.call, detail\.result\)/);
  assert.match(panel, /\{subagent !== undefined && \(\s*<SubagentTranscript\s+key=\{detail\.key\}\s+summary=\{subagent\.summary\}\s+settled=\{subagent\.settled\}\s+sessionRef=\{sessionRef\}\s+profile=\{profile\}\s+callId=\{detail\.call\.callId\}/);
  assert.doesNotMatch(panel, /detail\.result\?\.subagent !== undefined/);
  const pane = code(component('ViewPane.tsx'));
  assert.match(pane, /<ActivityPanel[\s\S]*?sessionRef=\{sessionRef\}\s+profile=\{profile\}/);
});

test('the cache size and the poll cadence are settings', () => {
  const config = JSON.parse(readFileSync(new URL('./config.json', import.meta.url), 'utf8'));
  assert.ok(Number.isInteger(config.subagent_transcript_cache_entries) && config.subagent_transcript_cache_entries >= 1);
  assert.equal(config.subagent_transcript_poll_ms, 2000);
  assert.equal(config.subagent_transcript_poll_max_delay_ms, 15000);
  assert.ok(config.subagent_transcript_poll_max_delay_ms >= config.subagent_transcript_poll_ms);
  assert.match(code(component('SubagentTranscript.tsx')), /createSubagentTranscriptStore\(appConfig\.subagent_transcript_cache_entries\)/);
});
