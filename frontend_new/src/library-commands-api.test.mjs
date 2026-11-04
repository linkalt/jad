import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}', {status: 200});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};

const {expandRootLibraryCommand, fetchRootLibrary} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

const json = (value, status = 200) => new Response(JSON.stringify(value), {status});
const empty = {prompts: [], templates: [], skills: []};

test('the catalog reads the custom commands, and an older server without them gives none', async () => {
  reply = () =>
    json({
      ...empty,
      commands: [
        {id: 'c1', kind: 'command', name: 'Commit', description: 'Commit', source: 'git/commit.md', command: 'git:commit', argument_hint: '<message>', takes_arguments: true},
        {id: 'c2', kind: 'command', name: 'Lint', description: '', source: 'lint.md', command: 'lint'},
      ],
    });
  const catalog = await fetchRootLibrary();
  assert.deepEqual(catalog.commands, [
    {id: 'c1', kind: 'command', name: 'Commit', description: 'Commit', source: 'git/commit.md', command: 'git:commit', argumentHint: '<message>', takesArguments: true},
    {id: 'c2', kind: 'command', name: 'Lint', description: '', source: 'lint.md', command: 'lint', takesArguments: false},
  ]);
  reply = () => json(empty);
  assert.deepEqual((await fetchRootLibrary()).commands, []);
});

test('a malformed command is refused', async () => {
  for (const commands of [
    'x',
    [{id: 'c', kind: 'prompt', name: 'n', description: '', source: 's', command: 'x'}],
    [{id: 'c', kind: 'command', name: 'n', description: '', source: 's', command: ''}],
    [{id: 'c', kind: 'command', name: 'n', description: '', source: 's', command: 'x', takes_arguments: 'yes'}],
  ]) {
    reply = () => json({...empty, commands});
    await assert.rejects(fetchRootLibrary(), /invalid library data/);
  }
});

test('expand posts the arguments and returns the text, or the server refusal', async () => {
  requests.length = 0;
  reply = () => json({command: 'git:commit', text: 'Commit: fix'});
  assert.equal(await expandRootLibraryCommand('c 1', 'fix'), 'Commit: fix');
  assert.match(requests[0].url, /\/library\/command\/c%201\/expand$/);
  assert.equal(requests[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(requests[0].init.body), {arguments: 'fix'});
  reply = () => json({error: "the command's text is empty"}, 422);
  await assert.rejects(expandRootLibraryCommand('c1', ''), /text is empty/);
  reply = () => json({command: 'x'});
  await assert.rejects(expandRootLibraryCommand('c1', ''), /invalid command data/);
});
