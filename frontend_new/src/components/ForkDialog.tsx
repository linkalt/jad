/**
 * @file ForkDialog.tsx
 * @input A fork or duplicate request (what is copied and the proposed name), or null
 * @output A modal that asks for the new profile's or session's name (and, for a duplicate,
 *         whether the conversation comes along); Fork/Duplicate hands the answer back
 * @position Rendered by App for the transcript's fork buttons and the row menu's Duplicate
 */
import {useEffect, useState} from 'react';
import {Button, CheckboxInput, Dialog, HStack, Layout, LayoutContent, LayoutFooter, TextInput, VStack} from '@astryxdesign/core';
import {DialogHeader} from '@astryxdesign/core/Dialog';
import {forkNameProblem} from '../session-fork';

export type ForkDialogRequest = {
  /** fork: a new profile of the same session from a message; duplicate: a copy of a row. */
  mode: 'fork' | 'duplicate';
  /** What the new name names. */
  kind: 'profile' | 'session';
  /** What is copied, for the title (a profile or session label). */
  sourceLabel: string;
  /** The proposed name. */
  defaultName: string;
  /** The category a new profile goes in, shown before the name. */
  category?: string;
  /** What the fork keeps of the conversation, for the subtitle. */
  detail?: string;
};

export type ForkDialogAnswer = {name: string; title: string; copyState: boolean};

type ForkDialogProps = {
  request: ForkDialogRequest | null;
  /** True while the fork request is on its way: the buttons wait. */
  isBusy: boolean;
  onCancel: () => void;
  onSubmit: (answer: ForkDialogAnswer) => void;
};

export function ForkDialog({request, isBusy, onCancel, onSubmit}: ForkDialogProps) {
  const [name, setName] = useState('');
  const [title, setTitle] = useState('');
  const [copyState, setCopyState] = useState(true);
  const [touched, setTouched] = useState(false);
  useEffect(() => {
    setName(request?.defaultName ?? '');
    setTitle('');
    setCopyState(true);
    setTouched(false);
  }, [request]);
  if (request === null) return null;

  const problem = forkNameProblem(name, request.kind);
  const submit = () => {
    setTouched(true);
    if (problem !== null || isBusy) return;
    onSubmit({name, title, copyState: request.mode === 'fork' || copyState});
  };
  const action = request.mode === 'fork' ? 'Fork' : 'Duplicate';
  const heading = request.mode === 'fork' ? `Fork ${request.sourceLabel} into a new profile` : `Duplicate ${request.sourceLabel}`;
  const subtitle = request.mode === 'fork'
    ? `The new profile copies this profile's settings and lives in the same session${request.detail ? `; ${request.detail}` : ''}.`
    : request.kind === 'profile'
      ? 'A new profile in the same session with a copy of this profile\'s settings.'
      : 'A new session with a copy of this session\'s settings.';
  return (
    <Dialog isOpen onOpenChange={open => { if (!open) onCancel(); }} purpose="form" aria-label={heading}>
      <Layout
        header={<DialogHeader title={heading} subtitle={subtitle} />}
        content={
          <LayoutContent>
            <VStack gap={3}>
              <TextInput
                label={request.kind === 'profile' ? 'Profile name' : 'Session name'}
                description={request.kind === 'profile' && request.category ? `Saved as ${request.category}/${name || '…'}` : undefined}
                value={name}
                onChange={value => {
                  setName(value.trim());
                  setTouched(true);
                }}
                onEnter={submit}
                status={touched && problem !== null ? {type: 'error', message: problem} : undefined}
                hasAutoFocus
                autoComplete="off"
              />
              {request.kind === 'profile' ? (
                <TextInput
                  label="Display name"
                  description="Shown in the session list; empty shows the profile name."
                  isOptional
                  value={title}
                  onChange={setTitle}
                  onEnter={submit}
                  autoComplete="off"
                />
              ) : null}
              {request.mode === 'duplicate' ? (
                <CheckboxInput
                  label="Copy the conversation"
                  description={copyState ? 'The copy starts with the whole conversation so far.' : 'The copy starts with an empty conversation.'}
                  value={copyState}
                  onChange={setCopyState}
                />
              ) : null}
            </VStack>
          </LayoutContent>
        }
        footer={
          <LayoutFooter hasDivider>
            <HStack gap={2} justify="end" width="100%">
              <Button label="Cancel" variant="ghost" onClick={onCancel} />
              <Button label={action} variant="primary" onClick={submit} isDisabled={(touched && problem !== null) || isBusy} isLoading={isBusy} />
            </HStack>
          </LayoutFooter>
        }
      />
    </Dialog>
  );
}
