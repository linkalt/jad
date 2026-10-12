/**
 * @file StateContextDrawer.tsx
 * @input A prompt typed by the user and the preview/write callbacks for the session's state context
 * @output The composer drawer that previews and writes the state-context block
 * @position The surface for putting a user prompt into the session's state-context block
 */
import {useEffect, useRef, useState} from 'react';

import {Button, HStack, Text, TextArea, VStack} from '@astryxdesign/core';
import {CodeBlock} from '@astryxdesign/core/CodeBlock';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';

import type {SessionStateContextResult} from '../api';
import {stateContextPreviewView} from '../state-context-view';

export interface StateContextDrawerProps {
  /** True while a run, compaction or reset is in flight. */
  isDisabled: boolean;
  previewMaxHeightPx: number;
  onPreview: (prompt: string) => Promise<SessionStateContextResult>;
  onWrite: (prompt: string) => Promise<SessionStateContextResult>;
  onClose: () => void;
}

export function StateContextDrawer({isDisabled, previewMaxHeightPx, onPreview, onWrite, onClose}: StateContextDrawerProps) {
  const [prompt, setPrompt] = useState('');
  const [preview, setPreview] = useState<SessionStateContextResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<'preview' | 'write' | null>(null);
  const isMounted = useRef(true);
  const previewView = preview === null ? null : stateContextPreviewView(preview);

  useEffect(() => {
    isMounted.current = true;
    return () => {
      isMounted.current = false;
    };
  }, []);

  const changePrompt = (value: string): void => {
    setPrompt(value);
    setPreview(null);
    setError(null);
  };

  const run = async (
    kind: 'preview' | 'write',
    call: (text: string) => Promise<SessionStateContextResult>,
  ): Promise<void> => {
    setBusy(kind);
    setError(null);
    try {
      const result = await call(prompt);
      if (!isMounted.current) {
        return;
      }
      if (kind === 'write' && result.written) {
        onClose();
        return;
      }
      setPreview(result);
    } catch (caught) {
      if (isMounted.current) {
        setError(caught instanceof Error ? caught.message : String(caught));
      }
    } finally {
      if (isMounted.current) {
        setBusy(null);
      }
    }
  };

  return (
    <ChatComposerDrawer>
      <VStack gap={1} width="100%">
        <Text type="supporting" color="secondary">
          Write a state-context block now. Your prompt is placed after the header and before the session data, and stays in the request history until the next compaction.
        </Text>
        <TextArea
          label="Prompt (optional)"
          placeholder="Say what the model should keep in mind"
          value={prompt}
          rows={3}
          size="sm"
          isDisabled={isDisabled || busy !== null}
          onChange={changePrompt}
        />
        {previewView?.kind === 'empty' ? (
          <Text type="supporting" color="secondary">
            Nothing to write: no prompt and no data from the configured providers.
          </Text>
        ) : null}
        {previewView?.kind === 'block' ? (
          <VStack gap={1} width="100%">
            <Text type="label">Preview</Text>
            <CodeBlock
              code={previewView.text}
              language="text"
              size="sm"
              width="100%"
              container="card"
              maxHeight={previewMaxHeightPx}
              hasCopyButton
            />
            {previewView.summary === null ? null : (
              <Text type="supporting" color="secondary">
                {previewView.summary}
              </Text>
            )}
          </VStack>
        ) : null}
        {error !== null ? (
          <Text type="supporting" color="secondary">
            {error}
          </Text>
        ) : null}
        <HStack align="center" gap={1}>
          <Button
            label="Close"
            variant="ghost"
            size="sm"
            isDisabled={busy !== null}
            onClick={onClose}
          />
          <Button
            label="Preview"
            variant="secondary"
            size="sm"
            isLoading={busy === 'preview'}
            isDisabled={isDisabled || busy !== null}
            onClick={() => void run('preview', onPreview)}
          />
          <Button
            label="Write block"
            variant="primary"
            size="sm"
            isLoading={busy === 'write'}
            isDisabled={isDisabled || busy !== null}
            onClick={() => void run('write', onWrite)}
          />
        </HStack>
      </VStack>
    </ChatComposerDrawer>
  );
}
