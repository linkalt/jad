import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import React from 'react';
import {renderToStaticMarkup} from 'react-dom/server';
import {Markdown} from '@astryxdesign/core';

import {buildRunInput, userMessageImages} from './image-attachments.ts';

const LITERAL_ASTERISK_CASES = [
  'hello*',
  '*hello',
  'a *b',
  'hello**',
  'hello *',
  'hello*\nworld*',
];

test('buildRunInput and userMessageImages preserve trailing and unpaired asterisks', () => {
  for (const text of LITERAL_ASTERISK_CASES) {
    assert.equal(buildRunInput(text, []), text);
    assert.deepEqual(buildRunInput(text, [{dataUrl: 'data:image/png;base64,AA=='}]), [
      {type: 'text', text},
      {type: 'image', data: 'data:image/png;base64,AA=='},
    ]);
    assert.equal(userMessageImages({text}).text, text);
  }
});

test('non-streaming Markdown preserves trailing and unpaired asterisks', () => {
  for (const text of LITERAL_ASTERISK_CASES) {
    const htmlFalse = renderToStaticMarkup(
      React.createElement(Markdown, {isStreaming: false, density: 'compact'}, text),
    );
    const htmlDefault = renderToStaticMarkup(
      React.createElement(Markdown, {density: 'compact'}, text),
    );
    for (const part of text.split('\n')) {
      assert.ok(htmlFalse.includes(part), `isStreaming=false must keep "${part}" in ${htmlFalse}`);
      assert.ok(htmlDefault.includes(part), `default Markdown must keep "${part}" in ${htmlDefault}`);
    }
  }

  // Paired Markdown emphasis remains active while streaming-only trimming stays off.
  const italicHtml = renderToStaticMarkup(
    React.createElement(Markdown, {isStreaming: false, density: 'compact'}, '*hello*'),
  );
  assert.match(italicHtml, /<em>hello<\/em>/);

  // Contrast: isStreaming=true triggers Astryx trimStreamingArtifacts on trailing '*'.
  const streamingHtml = renderToStaticMarkup(
    React.createElement(Markdown, {isStreaming: true, density: 'compact'}, 'hello*'),
  );
  assert.equal(streamingHtml.includes('hello*'), false);
});

test('user-message and composer-preview MarkdownText sites explicitly pass isStreaming={false}', () => {
  const transcriptRow = readFileSync(new URL('./components/TranscriptRow.tsx', import.meta.url), 'utf8');
  const userBubble = transcriptRow.slice(
    transcriptRow.indexOf("message.sender === 'user'"),
    transcriptRow.indexOf('return <AssistantMessage'),
  );
  assert.match(
    userBubble,
    /<MarkdownText[^>]*isStreaming=\{false\}[^>]*>\{userImages\.text\}<\/MarkdownText>/,
  );

  const readOnly = readFileSync(new URL('./components/ReadOnlyTranscript.tsx', import.meta.url), 'utf8');
  const readOnlyUser = readOnly.slice(
    readOnly.indexOf("if (message.role === 'user')"),
    readOnly.indexOf('return <ReadOnlyBody'),
  );
  assert.match(readOnlyUser, /<MarkdownText[^>]*isStreaming={false}/);

  const preview = readFileSync(new URL('./components/ComposerPreview.tsx', import.meta.url), 'utf8');
  assert.match(preview, /<MarkdownText[^>]*isStreaming={false}/);
});
