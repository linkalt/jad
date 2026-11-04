/**
 * @file CommandArgumentsDialog.tsx
 * @input A custom command picked from the `/` menu whose text takes arguments, or null
 * @output A modal with one field for the arguments; Insert hands them back, Cancel hands back nothing
 * @position Rendered by ComposerPane; the composer asks the server to fill the command and inserts it
 */
import {useEffect, useState} from 'react';
import {Button, Dialog, HStack, Layout, LayoutContent, LayoutFooter, TextInput} from '@astryxdesign/core';
import {DialogHeader} from '@astryxdesign/core/Dialog';
import type {RootLibraryCommand} from '../api';

type CommandArgumentsDialogProps = {
  command: RootLibraryCommand | null;
  onCancel: () => void;
  onSubmit: (command: RootLibraryCommand, args: string) => void;
};

export function CommandArgumentsDialog({command, onCancel, onSubmit}: CommandArgumentsDialogProps) {
  const [args, setArgs] = useState('');
  useEffect(() => setArgs(''), [command]);
  if (command === null) return null;

  // Arguments may be empty: $ARGUMENTS then becomes nothing, as it does in jad run.
  const submit = () => onSubmit(command, args);
  return (
    <Dialog isOpen onOpenChange={open => { if (!open) onCancel(); }} purpose="form" aria-label={`Command /${command.command}`}>
      <Layout
        header={<DialogHeader title={`/${command.command}`} subtitle={command.description || command.name} />}
        content={
          <LayoutContent>
            <TextInput
              label="Arguments"
              description="Fills $ARGUMENTS, and $1 to $9 word by word; a double-quoted run is one word."
              placeholder={command.argumentHint}
              isOptional
              value={args}
              onChange={setArgs}
              onEnter={submit}
              hasAutoFocus
              autoComplete="off"
            />
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
