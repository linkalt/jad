/**
 * @file CatalogConfirmDialog.tsx
 * @input One pending question, the kind's own note on what the state box keeps, and
 *   the two answers
 * @output A modal question with Cancel focused, and — for a session delete — a
 *   checkbox that re-words the question and rides along with the answer
 * @position The one confirmation behind every question the catalog editor asks;
 *   the caller does the work, this only asks
 *
 * It is a `Dialog`, not an `AlertDialog`: an alert's description is a string and
 * its body has no slot, so it cannot hold the checkbox. Everything the alert gave
 * is kept — `role="alertdialog"`, Escape through the layer stack, the focus trap,
 * the return of focus to whatever opened it (`Dialog` remembers the trigger when
 * `isOpen` turns true), and Cancel as the `data-autofocus` target that `Dialog`
 * focuses after `showModal()`. Cancel comes first in the footer and in the tab
 * order for the same reason the alert put it there: it is the least destructive
 * answer, and Enter on an untouched question must not delete anything.
 *
 * The surface stays mounted and is opened with `isOpen` rather than being added
 * and removed with each question: an unmounted dialog never runs its own close,
 * so focus would fall on the page body instead of back to the button that asked.
 */
import {useId, useLayoutEffect, useRef, useState} from 'react';

import {Button, Dialog, HStack, Heading, Layout, LayoutContent, LayoutFooter, Text, VStack} from '@astryxdesign/core';
import {CheckboxInput} from '@astryxdesign/core/CheckboxInput';

import type {DeletePrompt} from '../catalog-editor-view';
import appConfig from '../config.json' with {type: 'json'};

/**
 * The box deletes transcripts *and* tool state, so the label says both. It is the
 * wording the checkbox carried in the editor, kept word for word.
 */
export const DELETE_STATE_LABEL = 'Also delete its transcripts and tool state when deleting';

export interface CatalogConfirmDialogProps {
  isOpen: boolean;
  /** The question, re-worded from the answer the checkbox currently holds. */
  prompt: (deleteState: boolean) => DeletePrompt;
  /**
   * What unticking the box means for this kind, and the reason it is offered at all:
   * null where the kind keeps no state, so no box is shown rather than a dead control.
   */
  stateNote: string | null;
  onCancel: () => void;
  onConfirm: (deleteState: boolean) => void;
}

export function CatalogConfirmDialog({isOpen, prompt, stateNote, onCancel, onConfirm}: CatalogConfirmDialogProps) {
  const [deleteState, setDeleteState] = useState(false);
  // Every open starts unticked, whatever the last answer was. A layout effect, so
  // the fresh answer is in place before the frame that shows the question.
  useLayoutEffect(() => {
    if (isOpen) setDeleteState(false);
  }, [isOpen]);
  const titleId = useId();
  const descriptionId = useId();
  const question = prompt(deleteState);
  // Held one frame past the close: `Dialog` hides the surface in an effect, so the
  // content re-renders once more after the answer, and it must not flip to the next
  // question while the old one is still painted.
  const held = useRef(question);
  if (isOpen) held.current = question;
  const shown = held.current;

  return (
    <Dialog
      isOpen={isOpen}
      onOpenChange={open => {
        if (!open) onCancel();
      }}
      width={appConfig.catalog_confirm_dialog_width_px}
      purpose="form"
      role="alertdialog"
      aria-labelledby={titleId}
      aria-describedby={descriptionId}
    >
      <Layout
        content={
          <LayoutContent padding={4}>
            <VStack gap={3}>
              <Heading level={2} id={titleId}>{shown.title}</Heading>
              <Text type="body" color="secondary" id={descriptionId}>{shown.description}</Text>
              {stateNote !== null && (
                <CheckboxInput label={DELETE_STATE_LABEL} description={stateNote} value={deleteState} onChange={setDeleteState} />
              )}
            </VStack>
          </LayoutContent>
        }
        footer={
          <LayoutFooter hasDivider>
            <HStack gap={2} justify="end" align="center" width="100%">
              <Button variant="ghost" label="Cancel" data-autofocus onClick={onCancel} />
              <Button variant="destructive" label={shown.actionLabel} onClick={() => onConfirm(deleteState)} />
            </HStack>
          </LayoutFooter>
        }
      />
    </Dialog>
  );
}
