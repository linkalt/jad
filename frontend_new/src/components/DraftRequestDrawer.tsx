/**
 * @file DraftRequestDrawer.tsx
 * @input The question a run is waiting on, from `WebUiMessage.pending`
 * @output The composer drawer that accepts, edits or declines it
 * @position The answer surface for native tools that must ask the user
 *
 * The drawer is deliberately not a modal: a run waiting for an answer belongs to
 * one profile row, and blocking the page would stop the user from working in
 * another. Nothing here gates the composer — the question simply sits above the
 * input until it is answered, and the run's own deadline closes it either way.
 *
 * The form is derived from the draft (see `draft-answer.ts`): one row per scalar,
 * one list editor per array (entries added, removed and reordered, never one
 * input per index), plus rows the user adds for what
 * the agent missed — a draft that missed the one thing the user cares about would
 * otherwise cost a decline and a whole new turn. Edits are local until they are
 * submitted, so a declined draft leaves the proposal untouched.
 */
import {useEffect, useMemo, useRef, useState} from 'react';

import {
  Button,
  HStack,
  SegmentedControl,
  SegmentedControlItem,
  StackItem,
  Text,
  TextArea,
  TextInput,
  VStack,
} from '@astryxdesign/core';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';

import type {WebUiInteraction} from '../api';
import {
  DRAFT_ADDED_FIELD_KINDS,
  canExtendDraft,
  draftDeadlineLabel,
  draftExtendLabel,
  draftFields,
  hasDraftChanges,
  hasJsonDraftChanges,
  reviewDraftAnswer,
  reviewDraftJsonAnswer,
  type DraftAddedField,
  type DraftAddedFieldKind,
  type DraftDecision,
  type DraftListItem,
} from '../draft-answer';
import appConfig from '../config.json';
import {DraftFieldRows} from './DraftListField';

export interface DraftRequestDrawerProps {
  pending: WebUiInteraction;
  /** True while the answer is on its way to the run. */
  isSubmitting: boolean;
  /** True while an extension is on its way to the run. */
  isExtending: boolean;
  /** A refusal from the endpoint, e.g. the question expired first. */
  error: string | null;
  /** How long the reason may be; the endpoint refuses longer ones. */
  reasonMaxLength: number;
  /** How often the "time left" line refreshes while the run waits. */
  deadlineTickMs: number;
  onAnswer: (body: {decision: DraftDecision; draft?: unknown; reason?: string}) => void;
  onExtend: () => void;
}

export function DraftRequestDrawer({
  pending,
  isSubmitting,
  isExtending,
  error,
  reasonMaxLength,
  deadlineTickMs,
  onAnswer,
  onExtend,
}: DraftRequestDrawerProps) {
  const [draftBase, setDraftBase] = useState<unknown>(pending.draft);
  const fields = useMemo(() => draftFields(draftBase), [draftBase]);
  const [edits, setEdits] = useState<Record<string, string>>({});
  const [added, setAdded] = useState<DraftAddedField[]>([]);
  const [lists, setLists] = useState<Record<string, DraftListItem[]>>({});
  const [jsonText, setJsonText] = useState(() => JSON.stringify(pending.draft, null, 2));
  const [isJsonMode, setIsJsonMode] = useState(false);
  const [reason, setReason] = useState('');
  const [isDeclining, setIsDeclining] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const [now, setNow] = useState(() => Date.now());
  const nextAddedId = useRef(0);
  const nextListItemId = useRef(0);

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), deadlineTickMs);
    return () => clearInterval(timer);
  }, [deadlineTickMs]);

  // A new question starts from its own proposal, never from the last answer's edits.
  useEffect(() => {
    setDraftBase(pending.draft);
    setEdits({});
    setAdded([]);
    setLists({});
    setJsonText(JSON.stringify(pending.draft, null, 2));
    setIsJsonMode(false);
    setReason('');
    setIsDeclining(false);
    setProblem(null);
  }, [pending.id]);

  const isEdited = isJsonMode
    ? hasJsonDraftChanges(jsonText, pending.draft)
    : JSON.stringify(draftBase) !== JSON.stringify(pending.draft) || hasDraftChanges(fields, edits, added, lists);
  const deadline = draftDeadlineLabel(pending.expires_at, now);
  const extendable = canExtendDraft(pending.expires_at, pending.max_expires_at, now);

  const submit = (decision: DraftDecision): void => {
    if (decision === 'update' && isJsonMode) {
      const reviewed = reviewDraftJsonAnswer(jsonText);
      if ('error' in reviewed) {
        setProblem(reviewed.error);
        return;
      }
      setProblem(null);
      onAnswer(reviewed.body);
      return;
    }
    const reviewed = reviewDraftAnswer({
      draft: draftBase,
      fields,
      edits,
      added,
      lists,
      decision,
      reason,
      reasonMaxLength,
    });
    if ('error' in reviewed) {
      setProblem(reviewed.error);
      return;
    }
    setProblem(null);
    onAnswer(reviewed.body);
  };

  const switchToJson = (): void => {
    const reviewed = reviewDraftAnswer({
      draft: draftBase,
      fields,
      edits,
      added,
      lists,
      decision: 'update',
      reason: '',
      reasonMaxLength,
    });
    if ('error' in reviewed) {
      setProblem(reviewed.error);
      return;
    }
    const nextDraft = reviewed.body.draft ?? draftBase;
    setDraftBase(nextDraft);
    setEdits({});
    setAdded([]);
    setLists({});
    setJsonText(JSON.stringify(nextDraft, null, 2));
    setIsJsonMode(true);
    setProblem(null);
  };

  const switchToFields = (): void => {
    const reviewed = reviewDraftJsonAnswer(jsonText);
    if ('error' in reviewed) {
      setProblem(reviewed.error);
      return;
    }
    setDraftBase(reviewed.body.draft);
    setEdits({});
    setAdded([]);
    setLists({});
    setIsJsonMode(false);
    setProblem(null);
  };

  const editTo = (key: string, value: string): void => {
    setEdits((previous) => ({...previous, [key]: value}));
  };
  const addField = (): void => {
    nextAddedId.current += 1;
    setAdded((previous) => [
      ...previous,
      {id: `added-${nextAddedId.current}`, name: '', kind: 'string', text: ''},
    ]);
  };
  const updateAdded = (id: string, patch: Partial<DraftAddedField>): void => {
    setAdded((previous) => previous.map((field) => (field.id === id ? {...field, ...patch} : field)));
  };
  const removeAdded = (id: string): void => {
    setAdded((previous) => previous.filter((field) => field.id !== id));
  };

  return (
    <ChatComposerDrawer>
      {/*
        A long draft — many fields, or a tall JSON box — used to grow this drawer
        past the pane with no scrollbar. The body now caps and scrolls. `Stack`
        has no `maxHeight` prop and StyleX takes literals only, so the configured
        cap arrives inline, the way the pane header's z-index does.
      */}
      <VStack
        gap={2}
        width="100%"
        isScrollable
        style={{maxHeight: appConfig.composer_drawer_max_height_px}}
      >
        <VStack gap={1} width="100%">
          <Text type="label">{pending.title ?? 'Confirm what the agent understood'}</Text>
          <Text type="supporting" color="secondary">
            {`${pending.tool} is waiting for your answer${deadline === '' ? '' : ` — ${deadline}`}. Accept to run as drafted, edit fields or the full JSON and accept to run with your version, or decline with a reason. Typing below is not blocked.`}
          </Text>
          {pending.extend_ms > 0 ? (
            <HStack align="center" gap={1}>
              <Button
                label={`Extend ${draftExtendLabel(pending.extend_ms)}`}
                variant="ghost"
                size="sm"
                isLoading={isExtending}
                isDisabled={isSubmitting || isExtending || !extendable}
                onClick={onExtend}
              />
              {extendable ? null : (
                <Text type="supporting" color="secondary">
                  No more time can be added.
                </Text>
              )}
            </HStack>
          ) : null}
        </VStack>
        {isJsonMode ? (
          <VStack gap={1} width="100%">
            <TextArea
              label="Full draft (JSON)"
              value={jsonText}
              rows={8}
              size="sm"
              isDisabled={isSubmitting}
              onChange={(value: string) => setJsonText(value)}
            />
            <Button
              label="Use field editor"
              variant="ghost"
              size="sm"
              isDisabled={isSubmitting}
              onClick={switchToFields}
            />
          </VStack>
        ) : (
          <>
            {fields.length === 0 ? (
              <Text type="supporting" color="secondary">
                The draft has no fields to confirm; accepting proceeds with it as it stands, or add a
                field the agent missed.
              </Text>
            ) : (
              <DraftFieldRows
                base={draftBase}
                edits={edits}
                lists={lists}
                isDisabled={isSubmitting}
                nextItemId={() => {
                  nextListItemId.current += 1;
                  return `item-${nextListItemId.current}`;
                }}
                onEdit={editTo}
                onList={(key, items) => setLists((previous) => ({...previous, [key]: items}))}
              />
            )}
            {added.map((field) => (
              <VStack key={field.id} gap={1} width="100%">
                <HStack gap={1} align="end" width="100%">
                  <StackItem size="fill">
                    <TextInput
                      label="New field"
                      placeholder="field name"
                      value={field.name}
                      size="sm"
                      isDisabled={isSubmitting}
                      onChange={(value: string) => updateAdded(field.id, {name: value})}
                    />
                  </StackItem>
                  <Button
                    label="Remove field"
                    variant="ghost"
                    size="sm"
                    isDisabled={isSubmitting}
                    onClick={() => removeAdded(field.id)}
                  />
                </HStack>
                <HStack gap={1} align="end" width="100%">
                  <StackItem size="fill">
                    {field.kind === 'json' || field.kind === 'string' ? (
                      <TextArea
                        label={field.kind === 'json' ? 'Value (JSON)' : 'Value'}
                        value={field.text}
                        rows={field.kind === 'json' ? 3 : 2}
                        size="sm"
                        isDisabled={isSubmitting}
                        onChange={(value: string) => updateAdded(field.id, {text: value})}
                      />
                    ) : (
                      <TextInput
                        label="Value"
                        value={field.text}
                        size="sm"
                        isDisabled={isSubmitting}
                        onChange={(value: string) => updateAdded(field.id, {text: value})}
                      />
                    )}
                  </StackItem>
                  <SegmentedControl
                    label="New field type"
                    size="sm"
                    value={field.kind}
                    onChange={(value) => updateAdded(field.id, {kind: value as DraftAddedFieldKind})}
                  >
                    {DRAFT_ADDED_FIELD_KINDS.map((kind) => (
                      <SegmentedControlItem key={kind.id} value={kind.id} label={kind.label} />
                    ))}
                  </SegmentedControl>
                </HStack>
              </VStack>
            ))}
            <HStack gap={1} align="center">
              <Button
                label="Add field"
                variant="ghost"
                size="sm"
                isDisabled={isSubmitting}
                onClick={addField}
              />
              <Button
                label="Edit full JSON"
                variant="ghost"
                size="sm"
                isDisabled={isSubmitting}
                onClick={switchToJson}
              />
              {added.length > 0 ? (
                <Text type="supporting" color="secondary">
                  Added fields travel with Update &amp; accept.
                </Text>
              ) : null}
            </HStack>
          </>
        )}
        {isDeclining ? (
          <TextArea
            label="Why are you declining?"
            placeholder="Say what to do instead, so the agent can redraft"
            value={reason}
            rows={2}
            size="sm"
            maxLength={reasonMaxLength}
            isDisabled={isSubmitting}
            onChange={(value: string) => setReason(value)}
          />
        ) : null}
        {problem !== null || error !== null ? (
          <Text type="supporting" color="secondary">
            {problem ?? error}
          </Text>
        ) : null}
        <HStack align="center" gap={1}>
          {/*
            One primary action, labelled with what it will actually send: editing a
            field or adding one turns Accept into Update & accept, so a change can
            never be dropped by clicking the button next to the right one.
          */}
          {isEdited ? (
            <Button
              label="Update & accept"
              variant="primary"
              size="sm"
              isLoading={isSubmitting}
              isDisabled={isSubmitting}
              onClick={() => submit('update')}
            />
          ) : (
            <Button
              label="Accept"
              variant="primary"
              size="sm"
              isLoading={isSubmitting}
              isDisabled={isSubmitting}
              onClick={() => submit('accept')}
            />
          )}
          {isDeclining ? (
            <Button
              label="Send decline"
              variant="destructive"
              size="sm"
              isDisabled={isSubmitting || reason.trim().length === 0}
              onClick={() => submit('decline')}
            />
          ) : (
            <Button
              label="Decline…"
              variant="ghost"
              size="sm"
              isDisabled={isSubmitting}
              onClick={() => setIsDeclining(true)}
            />
          )}
        </HStack>
      </VStack>
    </ChatComposerDrawer>
  );
}
