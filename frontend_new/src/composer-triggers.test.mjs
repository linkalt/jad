import assert from 'node:assert/strict';
import {existsSync} from 'node:fs';
import test from 'node:test';

import {
  BUILTIN_MENTION_FILES,
  createComposerTriggers,
  createSessionFileSource,
  inactiveSlashTrigger,
  isLeadingSlashAtCaret,
  mentionItems,
  rankMentionFiles,
  resolveMentionFiles,
  slashTrigger,
} from './composer-triggers.ts';

const FILES = [
  'frontend/src/App.tsx',
  'frontend/src/components/ComposerPane.tsx',
  'backend/src/tools/native/edit.ts',
  'frontend/src/row-heights.ts',
];

test('a configured list replaces the built-in one only once it has entries', () => {
  assert.deepEqual(resolveMentionFiles([], BUILTIN_MENTION_FILES), BUILTIN_MENTION_FILES);
  assert.deepEqual(resolveMentionFiles(undefined, ['a.ts']), ['a.ts']);
  assert.deepEqual(resolveMentionFiles(['x.ts'], ['a.ts']), ['x.ts']);
});

test('an empty query keeps the list order and respects the cap', () => {
  assert.deepEqual(rankMentionFiles(FILES, '', 10), FILES);
  assert.deepEqual(rankMentionFiles(FILES, '  ', 2), FILES.slice(0, 2));
});

test('a file name beats the directories it sits in', () => {
  assert.deepEqual(rankMentionFiles(FILES, 'composer', 10), [
    'frontend/src/components/ComposerPane.tsx',
  ]);
  // 'native' only appears in a directory, so the match still works but ranks last.
  assert.deepEqual(rankMentionFiles(FILES, 'native', 10), ['backend/src/tools/native/edit.ts']);
});

test('an exact path wins, and matching is case-insensitive', () => {
  assert.deepEqual(rankMentionFiles(FILES, 'frontend/src/row-heights.ts', 10), [
    'frontend/src/row-heights.ts',
  ]);
  assert.deepEqual(rankMentionFiles(FILES, 'TSX', 10), [
    'frontend/src/App.tsx',
    'frontend/src/components/ComposerPane.tsx',
  ]);
});

test('a query with no match returns nothing, and the cap applies to matches', () => {
  assert.deepEqual(rankMentionFiles(FILES, 'zzz', 10), []);
  assert.deepEqual(rankMentionFiles(FILES, 'ts', 2), [
    'frontend/src/App.tsx',
    'frontend/src/components/ComposerPane.tsx',
  ]);
});

test('the menu items carry the path as both id and label', () => {
  assert.deepEqual(mentionItems(FILES, 'row', 10), [
    {id: 'frontend/src/row-heights.ts', label: 'frontend/src/row-heights.ts'},
  ]);
});

test('the @ trigger searches the list, bootstraps it, and inserts a path chip', () => {
  const [trigger] = createComposerTriggers({files: FILES, maxResults: 2});
  assert.equal(trigger.character, '@');
  assert.equal(trigger.menuLabel, 'Files');
  assert.equal(trigger.emptySearchText, 'No matching files');
  assert.deepEqual(trigger.searchSource.bootstrap(), [
    {id: 'frontend/src/App.tsx', label: 'frontend/src/App.tsx'},
    {id: 'frontend/src/components/ComposerPane.tsx', label: 'frontend/src/components/ComposerPane.tsx'},
  ]);
  assert.deepEqual(trigger.searchSource.search('row'), [
    {id: 'frontend/src/row-heights.ts', label: 'frontend/src/row-heights.ts'},
  ]);
  // The chip shows the path; the serialized value is the reference itself.
  assert.deepEqual(trigger.onSelect({id: 'frontend/src/App.tsx', label: 'frontend/src/App.tsx'}), {
    value: '@frontend/src/App.tsx',
    label: 'frontend/src/App.tsx',
    variant: 'blue',
  });
});

test('without a slash option the composer has only the @ trigger', () => {
  assert.equal(createComposerTriggers({files: FILES, maxResults: 5}).length, 1);
});

test('the slash trigger hands the selection to onPick and inserts no text', () => {
  const picked = [];
  const source = {search: () => [], bootstrap: () => []};
  const triggers = createComposerTriggers({
    files: FILES,
    maxResults: 5,
    slash: {source, onPick: selection => picked.push(selection)},
  });
  assert.equal(triggers.length, 2);
  const slash = triggers[1];
  assert.equal(slash.character, '/');
  assert.equal(slash.searchSource, source);
  const selection = {type: 'command', name: 'compact'};
  assert.equal(slash.onSelect({id: 'compact', label: '/compact', auxiliaryData: selection}), '');
  assert.deepEqual(picked, [selection]);
  assert.equal(slash.onSelect({id: 'x', label: '/x'}), '', 'an item without a selection inserts nothing');
  assert.equal(picked.length, 1);
});

test('a folder is ranked by its own name, and its trailing slash is not part of it', () => {
  const paths = ['frontend/src/components/', 'frontend/src/components/Card.tsx', 'src/', 'docs/src-notes.md'];
  assert.deepEqual(rankMentionFiles(paths, 'components', 10), [
    'frontend/src/components/',
    'frontend/src/components/Card.tsx',
  ]);
  assert.deepEqual(rankMentionFiles(paths, 'src', 10), ['src/', 'docs/src-notes.md', 'frontend/src/components/', 'frontend/src/components/Card.tsx']);
  assert.deepEqual(rankMentionFiles(paths, 'src/', 10), ['src/', 'frontend/src/components/', 'frontend/src/components/Card.tsx']);
});

test('every built-in path is relative, unique, and exists in this repository', () => {
  assert.ok(BUILTIN_MENTION_FILES.length > 0);
  assert.equal(new Set(BUILTIN_MENTION_FILES).size, BUILTIN_MENTION_FILES.length);
  assert.deepEqual(
    BUILTIN_MENTION_FILES.filter(path => path.startsWith('/')),
    [],
  );
  // The list is checked in for this repository, so a stale entry is a bug the
  // test can catch: the paths are resolved two levels up from frontend_new/src.
  const repoRoot = new URL('../../', import.meta.url);
  assert.deepEqual(
    BUILTIN_MENTION_FILES.filter(path => !existsSync(new URL(path, repoRoot))),
    [],
  );
});

test('the session source lists what its loader returns, and filters it locally', async () => {
  let loads = 0;
  const source = createSessionFileSource({
    loadFiles: async () => {
      loads += 1;
      return FILES;
    },
    fallbackFiles: ['fallback.ts'],
    maxResults: 10,
  });

  assert.deepEqual((await source.bootstrap()).map(item => item.id), FILES);
  assert.deepEqual(
    (await source.search('composer')).map(item => item.id),
    ['frontend/src/components/ComposerPane.tsx'],
  );
  assert.equal(loads, 1, 'search answers from the list bootstrap fetched');
});

test('every menu open asks the server again, so new files appear', async () => {
  let version = 1;
  const source = createSessionFileSource({
    loadFiles: async () => [`v${version}.ts`],
    fallbackFiles: [],
    maxResults: 10,
  });

  assert.deepEqual((await source.bootstrap()).map(item => item.id), ['v1.ts']);
  version = 2;
  assert.deepEqual((await source.bootstrap()).map(item => item.id), ['v2.ts']);
});

test('a search before bootstrap still loads once', async () => {
  let loads = 0;
  const source = createSessionFileSource({
    loadFiles: async () => {
      loads += 1;
      return FILES;
    },
    fallbackFiles: [],
    maxResults: 10,
  });

  assert.deepEqual(
    (await source.search('row-heights')).map(item => item.id),
    ['frontend/src/row-heights.ts'],
  );
  assert.equal(loads, 1);
});

test('a failing listing falls back to the static list instead of an empty menu', async () => {
  let offline = true;
  const source = createSessionFileSource({
    loadFiles: async () => {
      if (offline) {
        throw new Error('listing unavailable');
      }
      return FILES;
    },
    fallbackFiles: ['fallback.ts'],
    maxResults: 10,
  });

  assert.deepEqual((await source.bootstrap()).map(item => item.id), ['fallback.ts']);
  offline = false;
  assert.deepEqual((await source.bootstrap()).map(item => item.id), FILES);
});

test('cancel aborts the request in flight and the menu keeps a usable list', async () => {
  let seen;
  const source = createSessionFileSource({
    loadFiles: signal => {
      seen = signal;
      return new Promise((_resolve, reject) => {
        signal.addEventListener('abort', () => reject(new Error('aborted')), {once: true});
      });
    },
    fallbackFiles: ['fallback.ts'],
    maxResults: 10,
  });

  const pending = source.bootstrap();
  assert.equal(seen.aborted, false);
  source.cancel();
  assert.equal(seen.aborted, true);
  assert.deepEqual((await pending).map(item => item.id), ['fallback.ts']);
});

test('the results cap applies to a fetched list too', async () => {
  const source = createSessionFileSource({
    loadFiles: async () => FILES,
    fallbackFiles: [],
    maxResults: 2,
  });
  assert.deepEqual((await source.bootstrap()).map(item => item.id), FILES.slice(0, 2));
});

test('a trigger built over a session source reports its loading state', async () => {
  const triggers = createComposerTriggers({
    files: ['static.ts'],
    maxResults: 10,
    sessionFiles: async () => FILES,
  });
  const mention = triggers.find(trigger => trigger.character === '@');
  assert.ok(mention);
  assert.equal(mention.loadingText, 'Loading files…');
  assert.deepEqual((await mention.searchSource.bootstrap()).map(item => item.id), FILES);
});

test('without a session loader the trigger stays on the static list', async () => {
  const triggers = createComposerTriggers({files: FILES, maxResults: 10});
  const mention = triggers.find(trigger => trigger.character === '@');
  assert.deepEqual((await mention.searchSource.bootstrap()).map(item => item.id), FILES);
});

test('inactiveSlashTrigger provides a stable non-matching placeholder trigger', () => {
  const first = inactiveSlashTrigger();
  const second = inactiveSlashTrigger();
  assert.equal(first, second);
  assert.equal(first.character, '');
  assert.deepEqual(first.searchSource.search('plan'), []);
  assert.deepEqual(first.searchSource.bootstrap(), []);
  assert.equal(first.onSelect({id: 'plan', label: '/plan'}), '');

  const source = {search: () => [], bootstrap: () => []};
  const triggers = createComposerTriggers({
    files: FILES,
    maxResults: 5,
    slash: {source, onPick: () => {}},
    slashActive: false,
  });
  assert.equal(triggers.length, 2);
  assert.equal(triggers[0].character, '@');
  assert.equal(triggers[1], first);
});

test('isLeadingSlashAtCaret allows / only at index 0 of the composer draft', async () => {
  const {JSDOM} = await import('jsdom');
  const dom = new JSDOM('<!doctype html><html><body><div id="ed" contenteditable="true"></div></body></html>', {
    url: 'http://localhost/',
  });
  const prevWindow = globalThis.window;
  const prevDocument = globalThis.document;
  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
  try {
    const editable = document.getElementById('ed');
    const setCaret = (node, offset) => {
      const range = document.createRange();
      range.setStart(node, offset);
      range.collapse(true);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
    };

    // Positive cases: "/" at index 0, leading "/pl", "/mcp:foo/bar", "/hello" (caret at 1), single leading block
    editable.textContent = '/';
    setCaret(editable.firstChild, 1);
    assert.equal(isLeadingSlashAtCaret(editable), true);

    editable.textContent = '/pl';
    setCaret(editable.firstChild, 3);
    assert.equal(isLeadingSlashAtCaret(editable), true);

    editable.textContent = '/mcp:server/tool';
    setCaret(editable.firstChild, editable.firstChild.textContent.length);
    assert.equal(isLeadingSlashAtCaret(editable), true);

    editable.textContent = '/hello';
    setCaret(editable.firstChild, 1);
    assert.equal(isLeadingSlashAtCaret(editable), true, 'typing / at offset 0 of existing text');

    editable.innerHTML = '<div>/plan</div>';
    setCaret(editable.firstChild.firstChild, 5);
    assert.equal(isLeadingSlashAtCaret(editable), true, 'single wrapped leading block');

    // Negative cases: leading space, mid-draft, newline (<br> or block), token chip, after space in /cmd, prepended char
    for (const [label, setup] of [
      ['leading space', () => {
        editable.textContent = ' /';
        setCaret(editable.firstChild, 2);
      }],
      ['mid-draft after word', () => {
        editable.textContent = 'hello /';
        setCaret(editable.firstChild, 7);
      }],
      ['after <br>', () => {
        editable.innerHTML = 'hello<br>/';
        setCaret(editable.lastChild, 1);
      }],
      ['after leading <br>', () => {
        editable.innerHTML = '<br>/';
        setCaret(editable.lastChild, 1);
      }],
      ['second block after text', () => {
        editable.innerHTML = '<div>hello</div><div>/</div>';
        setCaret(editable.lastChild.firstChild, 1);
      }],
      ['second block after <br>', () => {
        editable.innerHTML = '<div><br></div><div>/</div>';
        setCaret(editable.lastChild.firstChild, 1);
      }],
      ['second block after empty div', () => {
        editable.innerHTML = '<div></div><div>/</div>';
        setCaret(editable.lastChild.firstChild, 1);
      }],
      ['immediately after token chip', () => {
        editable.innerHTML = '<span data-astryx-token="" data-astryx-token-value="@src/App.tsx">App.tsx</span>/';
        setCaret(editable.lastChild, 1);
      }],
      ['after token chip and nbsp', () => {
        editable.innerHTML = '<span data-astryx-token="" data-astryx-token-value="@src/App.tsx">App.tsx</span>\u00A0/';
        setCaret(editable.lastChild, 2);
      }],
      ['after space inside /cmd', () => {
        editable.textContent = '/cmd /';
        setCaret(editable.firstChild, 6);
      }],
      ['prepended char before /pl', () => {
        editable.textContent = 'a/pl';
        setCaret(editable.firstChild, 1);
      }],
      ['caret inside token chip', () => {
        editable.innerHTML = '<span data-astryx-token="" data-astryx-token-value="/plan">/plan</span>';
        setCaret(editable.firstChild.firstChild, 5);
      }],
      ['caret inside dictation interim', () => {
        editable.innerHTML = '<span data-astryx-dictation-interim="">/plan</span>';
        setCaret(editable.firstChild.firstChild, 5);
      }],
      ['non-collapsed selection', () => {
        editable.textContent = '/plan';
        const range = document.createRange();
        range.setStart(editable.firstChild, 0);
        range.setEnd(editable.firstChild, 5);
        const selection = window.getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
      }],
      ['selection outside editable', () => {
        const outside = document.createElement('div');
        outside.textContent = '/plan';
        document.body.appendChild(outside);
        setCaret(outside.firstChild, 5);
        outside.remove();
      }],
    ]) {
      setup();
      assert.equal(isLeadingSlashAtCaret(editable), false, label);
    }
  } finally {
    globalThis.window = prevWindow;
    globalThis.document = prevDocument;
  }
});

test('ChatComposerInput capture-phase gating opens / only at index 0 and closes cleanly with and without @', async () => {
  const {JSDOM} = await import('jsdom');
  const dom = new JSDOM('<!doctype html><html><body><div id="root"></div></body></html>', {
    url: 'http://localhost/',
  });
  const savedGlobals = {
    window: globalThis.window,
    document: globalThis.document,
    HTMLElement: globalThis.HTMLElement,
    Node: globalThis.Node,
    InputEvent: globalThis.InputEvent,
    requestAnimationFrame: globalThis.requestAnimationFrame,
    cancelAnimationFrame: globalThis.cancelAnimationFrame,
    IS_REACT_ACT_ENVIRONMENT: globalThis.IS_REACT_ACT_ENVIRONMENT,
  };
  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
  globalThis.HTMLElement = dom.window.HTMLElement;
  globalThis.Node = dom.window.Node;
  globalThis.InputEvent = dom.window.InputEvent;
  globalThis.requestAnimationFrame = cb => setTimeout(cb, 0);
  globalThis.cancelAnimationFrame = id => clearTimeout(id);
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;

  try {
    const React = await import('react');
    const {useEffect, useMemo, useRef, useState, act} = React;
    const {createRoot} = await import('react-dom/client');
    const {flushSync} = await import('react-dom');
    const {ChatComposerInput} = await import('@astryxdesign/core');

    const slashSource = {
      search: query =>
        [{id: 'plan', label: '/plan', auxiliaryData: {type: 'command', name: 'plan'}}].filter(item =>
          item.label.includes(query),
        ),
      bootstrap: () => [{id: 'plan', label: '/plan', auxiliaryData: {type: 'command', name: 'plan'}}],
    };

    const seenMentionTriggers = new Set();

    function Harness({mentionEnabled}) {
      const rootRef = useRef(null);
      const [leadingSlash, setLeadingSlash] = useState(false);
      const leadingSlashRef = useRef(false);
      const [value, setValue] = useState('');

      const base = useMemo(() => {
        const activeSlash = slashTrigger(slashSource, () => {});
        const inertSlash = inactiveSlashTrigger();
        const [mention] = mentionEnabled
          ? createComposerTriggers({files: FILES, maxResults: 5})
          : [undefined];
        return {mention, activeSlash, inertSlash};
      }, [mentionEnabled]);

      const triggers = useMemo(() => {
        const list = base.mention ? [base.mention] : [];
        if (base.mention) seenMentionTriggers.add(base.mention);
        list.push(leadingSlash ? base.activeSlash : base.inertSlash);
        return list;
      }, [base, leadingSlash]);

      useEffect(() => {
        const root = rootRef.current;
        if (root === null) return;
        const onCaptureInput = () => {
          const editable = root.querySelector('[contenteditable="true"]');
          const next = editable !== null ? isLeadingSlashAtCaret(editable) : false;
          if (next !== leadingSlashRef.current) {
            leadingSlashRef.current = next;
            flushSync(() => setLeadingSlash(next));
          }
        };
        root.addEventListener('input', onCaptureInput, true);
        return () => root.removeEventListener('input', onCaptureInput, true);
      }, []);

      return React.createElement(ChatComposerInput, {
        ref: rootRef,
        value,
        onChange: setValue,
        triggers,
      });
    }

    const setCaret = (node, offset) => {
      const range = document.createRange();
      range.setStart(node, offset);
      range.collapse(true);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
    };
    const fireInput = editable =>
      act(async () => {
        editable.dispatchEvent(new window.InputEvent('input', {bubbles: true}));
        await Promise.resolve();
      });
    const isMenuOpen = editable => editable.getAttribute('aria-expanded') === 'true';

    for (const mentionEnabled of [true, false]) {
      seenMentionTriggers.clear();
      const container = document.getElementById('root');
      const root = createRoot(container);
      try {
        await act(() => root.render(React.createElement(Harness, {mentionEnabled})));
        const editable = container.querySelector('[contenteditable="true"]');

        // 1. Leading "/" opens slash listbox
        editable.textContent = '/pl';
        setCaret(editable.firstChild, 3);
        await fireInput(editable);
        assert.equal(isMenuOpen(editable), true, `slash menu must open for /pl (mention=${mentionEnabled})`);
        let listbox = document.querySelector('[role="listbox"]');
        assert.ok(listbox);
        assert.match(listbox.textContent, /\/plan/);

        // 2. Typing space ("/pl ") closes the open slash listbox cleanly (even without @)
        editable.textContent = '/pl ';
        setCaret(editable.firstChild, 4);
        await fireInput(editable);
        assert.equal(isMenuOpen(editable), false, `slash menu must close after space (mention=${mentionEnabled})`);

        // 3. Invalid positions must not open slash listbox (including emptySearchText)
        for (const [label, setup] of [
          ['leading space', () => {
            editable.textContent = ' /';
            setCaret(editable.firstChild, 2);
          }],
          ['mid-draft after word', () => {
            editable.textContent = 'hello /';
            setCaret(editable.firstChild, 7);
          }],
          ['after <br>', () => {
            editable.innerHTML = 'hello<br>/';
            setCaret(editable.lastChild, 1);
          }],
          ['after token chip', () => {
            editable.innerHTML = '<span data-astryx-token="" data-astryx-token-value="@src/App.tsx">App.tsx</span>/';
            setCaret(editable.lastChild, 1);
          }],
          ['after space in /cmd', () => {
            editable.textContent = '/cmd /';
            setCaret(editable.firstChild, 6);
          }],
        ]) {
          setup();
          await fireInput(editable);
          assert.equal(
            isMenuOpen(editable),
            false,
            `slash menu must stay closed for ${label} (mention=${mentionEnabled})`,
          );
          assert.equal(
            document.body.textContent.includes('No matching command or library item'),
            false,
            `emptySearchText must not render for ${label} (mention=${mentionEnabled})`,
          );
        }

        // 4. Typing "/" at offset 0 of existing text opens the menu
        editable.textContent = '/hello';
        setCaret(editable.firstChild, 1);
        await fireInput(editable);
        assert.equal(isMenuOpen(editable), true, `slash menu must open at index 0 of existing text (mention=${mentionEnabled})`);

        // 5. Prepending "a" before "/hello" ("a/hello") closes the menu cleanly
        editable.textContent = 'a/hello';
        setCaret(editable.firstChild, 1);
        await fireInput(editable);
        assert.equal(
          isMenuOpen(editable),
          false,
          `slash menu must close when text is inserted before / (mention=${mentionEnabled})`,
        );

        // 6. @ mention still opens after space and keeps its object identity across / toggles
        if (mentionEnabled) {
          assert.equal(seenMentionTriggers.size, 1, '@ trigger object identity must remain stable');
          editable.textContent = 'hello @App';
          setCaret(editable.firstChild, 10);
          await fireInput(editable);
          assert.equal(isMenuOpen(editable), true, '@ menu must still open after space');
          listbox = document.querySelector('[role="listbox"]');
          assert.ok(listbox);
          assert.match(listbox.textContent, /App\.tsx/);
        }
      } finally {
        await act(() => root.unmount());
      }
    }
  } finally {
    dom.window.close();
    Object.assign(globalThis, savedGlobals);
  }
});
