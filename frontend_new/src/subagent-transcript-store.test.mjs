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

test('the summary is the agent, a count of at least one step and whether the result omits the child\'s last text', () => {
  assert.deepEqual(validSubagentSummary({agent: 'explore', steps: 7}), {agent: 'explore', steps: 7});
  assert.deepEqual(validSubagentSummary({steps: 1}), {steps: 1});
  assert.deepEqual(validSubagentSummary({steps: 2, omitsLastText: true}), {steps: 2, omitsLastText: true});
  for (const bad of [undefined, null, 'x', {}, {steps: 0}, {steps: -1}, {steps: 1.5}, {steps: '7'}, {agent: 3, steps: 2}, {steps: [{id: 'a', status: 'completed', parts: []}]}, {steps: 2, omitsLastText: 'yes'}]) {
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

test('a server error and a malformed body are errors, not a broken transcript', async () => {
  reply = () => new Response(JSON.stringify({error: 'subagent transcript not found'}), {status: 404});
  await assert.rejects(() => fetchSubagentTranscript('s', 'unknown'), /subagent transcript not found/);
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

test('the store fetches nothing until load, once per key, and reopening reads the cache', async () => {
  let fetches = 0;
  const fetch = async () => {
    fetches += 1;
    return transcript;
  };
  const store = createSubagentTranscriptStore(4);
  const key = subagentTranscriptKey('s', 'p', 'c1', 7);
  assert.equal(store.peek(key), undefined);
  assert.equal(fetches, 0, 'peek never fetches');
  assert.equal(await store.load(key, fetch), transcript);
  assert.equal(fetches, 1, 'one request on first open');
  assert.equal(store.peek(key), transcript);
  assert.equal(await store.load(key, fetch), transcript);
  assert.equal(fetches, 1, 'reopening is served from the cache');
  // Another call is another request, and only when it is asked for.
  await store.load(subagentTranscriptKey('s', 'p', 'c2', 3), fetch);
  assert.equal(fetches, 2);
});

test('callers that ask while a request is in flight share it; a failure is retried', async () => {
  const store = createSubagentTranscriptStore(4);
  let fetches = 0;
  let settle;
  const slow = () => {
    fetches += 1;
    return new Promise((resolve, reject) => {
      settle = {resolve, reject};
    });
  };
  const first = store.load('k', slow);
  const second = store.load('k', slow);
  assert.equal(fetches, 1);
  settle.reject(new Error('boom'));
  await assert.rejects(first, /boom/);
  await assert.rejects(second, /boom/);
  assert.equal(store.peek('k'), undefined, 'a failure is not cached');
  const retry = store.load('k', slow);
  assert.equal(fetches, 2, 'retry asks again');
  settle.resolve(transcript);
  assert.equal(await retry, transcript);
});

test('the store keeps a bounded number of transcripts, dropping the least recently used', async () => {
  const store = createSubagentTranscriptStore(2);
  const make = id => async () => ({steps: [{id, status: 'completed', parts: []}]});
  await store.load('a', make('a'));
  await store.load('b', make('b'));
  store.peek('a');
  await store.load('c', make('c'));
  assert.equal(store.peek('b'), undefined);
  assert.ok(store.peek('a') && store.peek('c'));
});

test('a newer summary (more steps) is a different key, so a partial transcript is not shown as current', () => {
  assert.notEqual(subagentTranscriptKey('s', undefined, 'c', 3), subagentTranscriptKey('s', undefined, 'c', 4));
  assert.notEqual(subagentTranscriptKey('s', 'p', 'c', 3), subagentTranscriptKey('s', undefined, 'c', 3));
  assert.notEqual(subagentTranscriptKey('s', 'p', 'c', 3), subagentTranscriptKey('t', 'p', 'c', 3));
});

// No DOM in this suite, so what a closed transcript can run is pinned at the source.
const component = name => readFileSync(new URL(`./components/${name}`, import.meta.url), 'utf8');
const code = source => source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');

test('closed, the section is one button: no fetch, no mapping and no transcript render before it is open', () => {
  const view = code(component('SubagentTranscript.tsx'));
  const closed = view.slice(view.indexOf('export function SubagentTranscript('), view.indexOf('type Load'));
  assert.doesNotMatch(closed, /fetchSubagentTranscript|subagentTranscriptMessages|<ReadOnlyTranscript|transcriptStore|useEffect|validSubagentTranscript/);
  assert.match(closed, /useState\(false\)/);
  // History is not validated part by part, so the section checks the summary itself: a
  // malformed one (null, a zero or fractional count) renders nothing instead of throwing.
  assert.match(closed, /if \(sessionRef === undefined \|\| validSubagentSummary\(summary\) === undefined\) \{\s*return null;/);
  // The label reads the summary's count, and the toggle flips it.
  assert.match(closed, /\$\{isOpen \? 'Hide' : 'Show'\} Transcript · \$\{summary\.steps\} \$\{summary\.steps === 1 \? 'step' : 'steps'\}/);
  assert.match(closed, /onClick=\{\(\) => setIsOpen\(open => !open\)\}/);
  // Everything that costs something lives in the open component, mounted only while open.
  assert.equal((closed.match(/<OpenSubagentTranscript/g) ?? []).length, 1);
  assert.match(closed, /\{isOpen && \(\s*<OpenSubagentTranscript/);
  const open = view.slice(view.indexOf('function OpenSubagentTranscript'));
  assert.match(open, /fetchSubagentTranscript\(sessionRef, callId, profile\)/);
  // The tool result says whether it omits the child's last text; only then is that text kept.
  assert.match(open, /subagentTranscriptMessages\(transcript, \{omitsLastText\}\)/);
  assert.match(closed, /omitsLastText=\{summary\.omitsLastText === true\}/);
  assert.equal((open.match(/<ReadOnlyTranscript/g) ?? []).length, 1);
  // The fetch is one load per key, with a retry button on failure and no timer.
  assert.match(open, /transcriptStore\s*\.load\(key/);
  assert.match(open, /Retry/);
  assert.doesNotMatch(view, /setInterval|setTimeout|prefetch/);
});

test('the panel passes the session, profile and call id and keys the section by the open call', () => {
  const panel = code(component('ActivityPanel.tsx'));
  assert.match(panel, /<SubagentTranscript\s+key=\{detail\.key\}\s+summary=\{detail\.result\.subagent\}\s+sessionRef=\{sessionRef\}\s+profile=\{profile\}\s+callId=\{detail\.call\.callId\}/);
  const pane = code(component('ViewPane.tsx'));
  assert.match(pane, /<ActivityPanel[\s\S]*?sessionRef=\{sessionRef\}\s+profile=\{profile\}/);
});

test('the cache size is a setting', () => {
  const config = JSON.parse(readFileSync(new URL('./config.json', import.meta.url), 'utf8'));
  assert.ok(Number.isInteger(config.subagent_transcript_cache_entries) && config.subagent_transcript_cache_entries >= 1);
  assert.match(code(component('SubagentTranscript.tsx')), /createSubagentTranscriptStore\(appConfig\.subagent_transcript_cache_entries\)/);
});
