/**
 * @file JsonCodeEditor.tsx
 * @input The document text, its problems with character ranges, and a change callback
 * @output A syntax-coloured JSON editor with line numbers, underlined problems and a gutter marker
 * @position The text surface of the catalog editor; it owns CodeMirror and nothing else
 *
 * Tab is left to the browser so the editor never traps keyboard focus. Colours,
 * font and the focus ring come from Astryx tokens, so the editor follows the theme.
 */
import {useEffect, useRef} from 'react';

import {VStack} from '@astryxdesign/core';
import {defaultKeymap, history, historyKeymap} from '@codemirror/commands';
import {json} from '@codemirror/lang-json';
import {HighlightStyle, bracketMatching, syntaxHighlighting} from '@codemirror/language';
import {lintGutter, setDiagnostics, type Diagnostic} from '@codemirror/lint';
import {Compartment, EditorState} from '@codemirror/state';
import {EditorView, drawSelection, highlightActiveLine, keymap, lineNumbers} from '@codemirror/view';
import {tags} from '@lezer/highlight';

import {clampProblems, type DocumentProblem} from '../json-document';

const highlightStyle = HighlightStyle.define([
  {tag: tags.propertyName, color: 'var(--color-syntax-property)'},
  {tag: tags.string, color: 'var(--color-syntax-string)'},
  {tag: tags.number, color: 'var(--color-syntax-number)'},
  {tag: [tags.bool, tags.null], color: 'var(--color-syntax-constant)'},
  {tag: [tags.brace, tags.squareBracket, tags.punctuation, tags.separator], color: 'var(--color-syntax-punctuation)'},
  {tag: tags.invalid, color: 'var(--color-error)'},
]);

const editorTheme = EditorView.theme({
  '&': {
    height: '100%',
    color: 'var(--color-text-primary)',
    backgroundColor: 'var(--color-background-surface)',
    fontFamily: 'var(--font-family-code)',
    fontSize: 'var(--font-size-base)',
    borderRadius: 'var(--radius-element)',
    border: 'var(--border-width) solid var(--color-border-emphasized)',
  },
  '&.cm-focused': {
    outlineWidth: 'var(--focus-outline-width)',
    outlineStyle: 'var(--focus-outline-style)',
    outlineColor: 'var(--focus-outline-color)',
    outlineOffset: 'calc(var(--focus-outline-offset) * -1)',
  },
  '.cm-scroller': {overflow: 'auto', fontFamily: 'inherit'},
  '.cm-content': {caretColor: 'var(--color-text-primary)', padding: 'var(--spacing-2) 0'},
  '.cm-cursor': {borderLeftColor: 'var(--color-text-primary)'},
  '.cm-gutters': {
    backgroundColor: 'var(--color-background-muted)',
    color: 'var(--color-text-secondary)',
    border: 'none',
    borderRight: 'var(--border-width) solid var(--color-border)',
  },
  '.cm-activeLine': {backgroundColor: 'var(--color-background-muted)'},
  '.cm-activeLineGutter': {backgroundColor: 'transparent', color: 'var(--color-text-primary)'},
  '&.cm-focused .cm-selectionBackground, .cm-selectionBackground': {backgroundColor: 'var(--color-background-blue)'},
  '.cm-matchingBracket': {backgroundColor: 'var(--color-background-blue)', outline: 'none'},
  '.cm-lintRange-error': {
    backgroundImage: 'none',
    textDecoration: 'underline wavy var(--color-error)',
    textUnderlineOffset: 'var(--spacing-0-5)',
  },
  '.cm-lintRange-warning': {
    backgroundImage: 'none',
    textDecoration: 'underline wavy var(--color-warning)',
    textUnderlineOffset: 'var(--spacing-0-5)',
  },
  '.cm-lint-marker-error': {content: 'none', backgroundColor: 'var(--color-error)', borderRadius: 'var(--radius-full)', width: 'var(--spacing-2)', height: 'var(--spacing-2)'},
  '.cm-tooltip': {
    color: 'var(--color-text-primary)',
    backgroundColor: 'var(--color-background-popover)',
    border: 'var(--border-width) solid var(--color-border-emphasized)',
    borderRadius: 'var(--radius-element)',
    fontFamily: 'var(--font-family-body)',
  },
  '.cm-diagnostic': {padding: 'var(--spacing-1) var(--spacing-2)'},
  '.cm-diagnostic-error': {borderLeft: 'var(--spacing-1) solid var(--color-error)'},
});

function toDiagnostics(problems: readonly DocumentProblem[], length: number): Diagnostic[] {
  return clampProblems(problems, length).map(problem => ({...problem, severity: 'error'}));
}

export interface JsonCodeEditorProps {
  /** Names the editor for assistive technology. */
  label: string;
  value: string;
  problems: readonly DocumentProblem[];
  isReadOnly?: boolean;
  /** A pixel count, or a CSS length such as min(420px, 40dvh). */
  height: number | string;
  onChange: (value: string) => void;
}

const editableCompartment = new Compartment();

function editableExtensions(isReadOnly: boolean) {
  return [EditorState.readOnly.of(isReadOnly), EditorView.editable.of(!isReadOnly)];
}

export function JsonCodeEditor({label, value, problems, isReadOnly = false, height, onChange}: JsonCodeEditorProps) {
  const hostRef = useRef<HTMLElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return undefined;
    const view = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: value,
        extensions: [
          lineNumbers(),
          history(),
          drawSelection(),
          highlightActiveLine(),
          bracketMatching(),
          json(),
          syntaxHighlighting(highlightStyle),
          lintGutter(),
          keymap.of([...defaultKeymap, ...historyKeymap]),
          editorTheme,
          EditorView.lineWrapping,
          EditorView.contentAttributes.of({'aria-label': label, 'aria-multiline': 'true'}),
          editableCompartment.of(editableExtensions(isReadOnly)),
          EditorView.updateListener.of(update => {
            if (update.docChanged) onChangeRef.current(update.state.doc.toString());
          }),
        ],
      }),
    });
    viewRef.current = view;
    return () => {
      view.destroy();
      viewRef.current = null;
    };
    // Only the label needs a new view; the read-only state is reconfigured in place so undo history survives a save.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [label]);

  useEffect(() => {
    viewRef.current?.dispatch({effects: editableCompartment.reconfigure(editableExtensions(isReadOnly))});
  }, [isReadOnly, label]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    const current = view.state.doc.toString();
    if (current !== value) {
      view.dispatch({changes: {from: 0, to: current.length, insert: value}});
    }
  }, [value, label]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    view.dispatch(setDiagnostics(view.state, toDiagnostics(problems, view.state.doc.length)));
  }, [problems, value, label]);

  return <VStack ref={hostRef} height={height} width="100%" />;
}
