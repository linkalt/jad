/**
 * @file ResourceVariablesDialog.tsx
 * @input An MCP resource template picked from the `/` menu, or null
 * @output A modal with one field per URI-template variable; Insert hands the values back, Cancel hands back nothing
 * @position Rendered by ComposerPane; the composer reads the filled resource and inserts its text
 */
import {useEffect, useState} from 'react';
import {Button, Dialog, HStack, Layout, LayoutContent, LayoutFooter, TextInput, VStack} from '@astryxdesign/core';
import {DialogHeader} from '@astryxdesign/core/Dialog';
import type {MCPResource} from '../api';

type ResourceVariablesDialogProps = {
  resource: MCPResource | null;
  onCancel: () => void;
  onSubmit: (resource: MCPResource, values: Record<string, string>) => void;
};

export function ResourceVariablesDialog({resource, onCancel, onSubmit}: ResourceVariablesDialogProps) {
  const [values, setValues] = useState<Record<string, string>>({});
  useEffect(() => setValues({}), [resource]);
  if (resource === null) return null;

  // A variable left empty is undefined, which the URI template expands to nothing.
  const submit = () => onSubmit(resource, values);
  const title = resource.title ?? resource.name;
  return (
    <Dialog isOpen onOpenChange={open => { if (!open) onCancel(); }} purpose="form" aria-label={`MCP resource ${title}`}>
      <Layout
        header={<DialogHeader title={title} subtitle={resource.description ?? resource.uriTemplate} />}
        content={
          <LayoutContent>
            <VStack gap={3}>
              {resource.variables.map((name, index) => (
                <TextInput
                  key={name}
                  label={name}
                  isOptional
                  value={values[name] ?? ''}
                  onChange={value => setValues(current => ({...current, [name]: value}))}
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
              <Button label="Insert" variant="primary" onClick={submit} />
            </HStack>
          </LayoutFooter>
        }
      />
    </Dialog>
  );
}
