/**
 * @file MarkdownText.tsx
 * @input Uses React, StyleX, Astryx Markdown, app config
 * @output Exports MarkdownText — Markdown renderer for transcript text
 * @position Shared UI component; used by App.tsx for user messages,
 *   assistant content parts, and reasoning content
 *
 * Transcript text is written with single line breaks (Shift+Enter in the
 * composer, or a newline in a model response). CommonMark reads a single
 * line break as a soft break and renders it as a space, which collapses a
 * multi-line message into one line. Astryx `Markdown` keeps the newline in
 * the rendered text node but styles prose with `white-space: normal`, so
 * keeping the break is a style-level decision rather than a parser one.
 *
 * `preserve_line_breaks` (config.json) is the switch:
 * - true (default) — line breaks render as authored.
 * - false — standard CommonMark soft-break collapsing.
 *
 * Fenced code blocks are unaffected either way: they already render with
 * preserved whitespace, and message text is never rewritten.
 */
import * as stylex from '@stylexjs/stylex';
import {Markdown} from '@astryxdesign/core';
import type {MarkdownProps} from '@astryxdesign/core';
import appConfig from '../config.json';

const styles = stylex.create({
  preserveLineBreaks: {
    whiteSpace: 'pre-wrap',
  },
});

export function MarkdownText({xstyle, ...markdownProps}: MarkdownProps) {
  return (
    <Markdown
      {...markdownProps}
      xstyle={
        appConfig.preserve_line_breaks
          ? [styles.preserveLineBreaks, xstyle]
          : xstyle
      }
    />
  );
}
