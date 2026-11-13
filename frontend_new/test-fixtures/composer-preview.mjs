/**
 * In-memory real-component layout regression; no app, storage, API or submission.
 * Use the existing Vite server, not an asset-generating build. In a test browser:
 * 1. Navigate to /@vite/client (a same-origin document without the app).
 * 2. Replace its content with a main#fixture and these stylesheet links:
 *    /src/index.css?direct and /virtual:stylex.css, preceded by the @layer order
 *    from index.html. Install Vite's /@react-refresh preamble before imports.
 * 3. Import this module via /test-fixtures/composer-preview.mjs and call mount().
 * 4. Call check(880), check(360), check(880). Each throws on outer overflow or
 *    inaccessible code. collect() returns computed styles and dimensions.
 * Baseline mutation: remove frame.contain in ComposerPreview.tsx, reload the
 * fixture/styles and repeat. Both widths fail (drawer row expands to code width).
 * Test-only dimensions/input are deliberately much wider than the scrollport.
 */
import React from 'react';
import {createRoot} from 'react-dom/client';
import {Theme} from '@astryxdesign/core/theme';
import {ChatComposer} from '@astryxdesign/core/Chat';
import {neutralTheme} from '../src/themes/neutral/neutral.js';
import {ComposerPreview} from '../src/components/ComposerPreview';

export const draft = 'Comparison ' + 'plainword'.repeat(80) +
  '\n\n```javascript\nconst long = "' + 'X'.repeat(800) + 'END";\n' +
  'word '.repeat(180) + 'END\nconsole.log("short");\n```\n\n- one\n- two';
let root;
export function mount() {
  const host = document.querySelector('#fixture');
  host.style.maxWidth = '100%';
  root = createRoot(host);
  render();
}
export function render(value = draft, open = true) {
  root.render(React.createElement(Theme, {theme: neutralTheme, mode: 'light'},
    React.createElement(ChatComposer, {
      value: '', onSubmit() {},
      drawer: open ? React.createElement(ComposerPreview, {value}) : undefined,
    })));
}
export function collect() {
  const markdown = document.querySelector('[role="document"]');
  const body = markdown.parentElement;
  const drawer = document.querySelector('.astryx-chat-composer-drawer');
  const pre = markdown.querySelector('pre');
  const scroll = pre.querySelector('[tabindex="0"]');
  const elements = {
    page: document.documentElement, composer: document.querySelector('.astryx-chat-composer'),
    drawer, grid: drawer.firstElementChild, drawerContent: drawer.firstElementChild.firstElementChild,
    frame: body.parentElement, body, markdown, fence: pre.parentElement, pre,
    scroll, inner: scroll.firstElementChild, code: pre.querySelector('code'),
  };
  return Object.fromEntries(Object.entries(elements).map(([name, element]) => {
    const css = getComputedStyle(element);
    const rect = element.getBoundingClientRect();
    return [name, {
      width: css.width, minWidth: css.minWidth, maxWidth: css.maxWidth,
      overflowX: css.overflowX, overflowY: css.overflowY, whiteSpace: css.whiteSpace,
      contain: css.contain, clientWidth: element.clientWidth, scrollWidth: element.scrollWidth,
      clientHeight: element.clientHeight, scrollHeight: element.scrollHeight,
      left: rect.left, right: rect.right,
    }];
  }));
}
export function check(width) {
  document.querySelector('#fixture').style.width = `${width}px`;
  const boxes = collect(); // computed styles/layout flush the width change
  for (const name of ['page', 'composer', 'drawer', 'grid', 'drawerContent', 'frame', 'body', 'fence', 'pre']) {
    const box = boxes[name];
    if (box.scrollWidth > box.clientWidth + 1 ||
        (name !== 'page' && box.right > boxes.drawer.right + 1)) {
      throw new Error(`${width}: outer overflow at ${name}: ${JSON.stringify(box)}`);
    }
  }
  const scroll = document.querySelector('pre [tabindex="0"]');
  scroll.scrollLeft = scroll.scrollWidth;
  if (scroll.scrollWidth <= scroll.clientWidth ||
      scroll.scrollLeft + scroll.clientWidth < scroll.scrollWidth - 1 ||
      boxes.code.whiteSpace !== 'pre' || !scroll.textContent.includes('END')) {
    throw new Error(`${width}: unwrapped code end is not internally reachable`);
  }
  return boxes;
}
