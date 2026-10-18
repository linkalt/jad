/**
 * @file json-document.ts
 * @input The text of a JSON document and a JSON pointer into it
 * @output Syntax problems with positions, and the character range of a pointer's target
 * @position Pure helpers behind the editor's diagnostics; no DOM and no CodeMirror
 */
import {parser} from '@lezer/json';
import type {SyntaxNode} from '@lezer/common';

export interface TextRange {
  from: number;
  to: number;
}

export interface DocumentProblem extends TextRange {
  message: string;
}

/** A target found by pointer: the value, and the key when it is an object member. */
export interface PointerTarget {
  value: TextRange;
  key?: TextRange;
  isContainer: boolean;
}

/** Widen an empty range to one character so the editor has something to underline. */
function visibleRange(from: number, to: number, length: number): TextRange {
  if (to > from) return {from, to: Math.min(to, length)};
  if (from < length) return {from, to: from + 1};
  return {from: Math.max(0, length - 1), to: length};
}

function firstErrorRange(text: string): TextRange | null {
  const cursor = parser.parse(text).cursor();
  do {
    if (cursor.type.isError) return visibleRange(cursor.from, cursor.to, text.length);
  } while (cursor.next());
  return null;
}

/**
 * `JSON.parse` decides whether the text is valid, so the editor never accepts what
 * the server's decoder would refuse. The Lezer tree only supplies the position,
 * and the message is the engine's own.
 */
export function syntaxProblem(text: string): DocumentProblem | null {
  try {
    JSON.parse(text);
    return null;
  } catch (caught) {
    const message = caught instanceof Error ? caught.message : String(caught);
    const range = firstErrorRange(text) ?? visibleRange(0, 0, text.length);
    return {...range, message};
  }
}

function childValue(node: SyntaxNode): SyntaxNode | null {
  for (let child: SyntaxNode | null = node.firstChild; child; child = child.nextSibling) {
    if (!child.type.isError) return child;
  }
  return null;
}

function unescapePointer(segment: string): string {
  return segment.replace(/~1/g, '/').replace(/~0/g, '~');
}

/**
 * The range a JSON pointer ("/tools/allow/0") names in `text`, or null when the
 * text does not have that path. A pointer of "" names the whole document.
 */
export function locatePointer(text: string, pointer: string): PointerTarget | null {
  const root = parser.parse(text).topNode;
  let node = childValue(root);
  if (!node) return null;
  let key: TextRange | undefined;
  const segments = pointer === '' ? [] : pointer.slice(1).split('/').map(unescapePointer);
  for (const segment of segments) {
    if (node.name === 'Object') {
      let found: SyntaxNode | null = null;
      for (let property: SyntaxNode | null = node.firstChild; property; property = property.nextSibling) {
        if (property.name !== 'Property') continue;
        const name = property.getChild('PropertyName');
        if (!name) continue;
        let propertyName: unknown;
        try {
          propertyName = JSON.parse(text.slice(name.from, name.to));
        } catch {
          continue;
        }
        if (propertyName === segment) {
          found = property;
          key = {from: name.from, to: name.to};
          break;
        }
      }
      if (!found) return null;
      const value = childValueAfterName(found);
      if (!value) return null;
      node = value;
    } else if (node.name === 'Array') {
      const index = Number(segment);
      if (!Number.isInteger(index) || index < 0) return null;
      let current = 0;
      let element: SyntaxNode | null = null;
      for (let child: SyntaxNode | null = node.firstChild; child; child = child.nextSibling) {
        if (child.type.isError || child.name === '[' || child.name === ']' || child.name === ',') continue;
        if (current === index) {
          element = child;
          break;
        }
        current += 1;
      }
      if (!element) return null;
      node = element;
      key = undefined;
    } else {
      return null;
    }
  }
  const isContainer = node.name === 'Object' || node.name === 'Array';
  return {value: {from: node.from, to: node.to}, ...(key ? {key} : {}), isContainer};
}

function childValueAfterName(property: SyntaxNode): SyntaxNode | null {
  let seenName = false;
  for (let child: SyntaxNode | null = property.firstChild; child; child = child.nextSibling) {
    if (child.name === 'PropertyName') {
      seenName = true;
      continue;
    }
    if (!seenName || child.type.isError || child.name === ':') continue;
    return child;
  }
  return null;
}

/** The part of a target a schema problem should underline: an opening bracket, a key or a scalar. */
export function problemRange(target: PointerTarget, length: number, preferKey: boolean): TextRange {
  if (preferKey && target.key) return visibleRange(target.key.from, target.key.to, length);
  if (target.isContainer) return visibleRange(target.value.from, target.value.from + 1, length);
  return visibleRange(target.value.from, target.value.to, length);
}

export interface ClampedProblem extends TextRange {
  message: string;
}

/** Keeps a problem's range inside a document that has changed since the problem was found. */
export function clampProblems(problems: readonly DocumentProblem[], length: number): ClampedProblem[] {
  return problems.map(problem => {
    const from = Math.min(Math.max(0, problem.from), length);
    const to = Math.min(Math.max(from, problem.to), length);
    return {from, to, message: problem.message};
  });
}
