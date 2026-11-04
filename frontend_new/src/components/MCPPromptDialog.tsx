/**
 * @file MCPPromptDialog.tsx
 * @input An MCP prompt picked from the `/` menu that takes arguments, or null
 * @output A modal with one field per argument; Insert hands the values back, Cancel hands back nothing
 * @position Rendered by ComposerPane; the composer fetches the filled prompt and inserts it
 */
import {useEffect, useState} from 'react';
import {Button, Dialog, HStack, Layout, LayoutContent, LayoutFooter, TextInput, VStack} from '@astryxdesign/core';
import {DialogHeader} from '@astryxdesign/core/Dialog';
import type {MCPPrompt} from '../api';
import {mcpPromptArgumentsReady} from '../slash-commands';

type MCPPromptDialogProps = {
  prompt: MCPPrompt | null;
  onCancel: () => void;
  onSubmit: (prompt: MCPPrompt, values: Record<string, string>) => void;
};

export function MCPPromptDialog({prompt, onCancel, onSubmit}: MCPPromptDialogProps) {
  const [values, setValues] = useState<Record<string, string>>({});
  useEffect(() => setValues({}), [prompt]);
  if (prompt === null) return null;

  const isReady = mcpPromptArgumentsReady(prompt, values);
  const submit = () => {
    if (isReady) onSubmit(prompt, values);
  };
  const title = prompt.title ?? prompt.name;

  return (
    <Dialog isOpen onOpenChange={open => { if (!open) onCancel(); }} purpose="form" aria-label={`MCP prompt ${title}`}>
      <Layout
        header={<DialogHeader title={title} subtitle={prompt.description ?? `MCP server ${prompt.server}`} />}
        content={
          <LayoutContent>
            <VStack gap={3}>
              {prompt.arguments.map((argument, index) => (
                <TextInput
                  key={argument.name}
                  label={argument.name}
                  description={argument.description}
                  isRequired={argument.required === true}
                  isOptional={argument.required !== true}
                  value={values[argument.name] ?? ''}
                  onChange={value => setValues(current => ({...current, [argument.name]: value}))}
                  onEnter={submit}
                  hasAutoFocus={index === 0}
                  autoComplete="off"
                />
              ))}
            </VStack>
          </LayoutContent>
        }
        footer={
          <LayoutFooter hasDivider>
            <HStack gap={2} justify="end" width="100%">
              <Button label="Cancel" variant="ghost" onClick={onCancel} />
              <Button label="Insert" variant="primary" isDisabled={!isReady} onClick={submit} />
            </HStack>
          </LayoutFooter>
        }
      />
    </Dialog>
  );
}
