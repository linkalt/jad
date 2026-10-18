/**
 * @file ApprovalDrawer.tsx
 * @input A gated tool call's preview, from `WebUiMessage.pending` with `kind: "approval"`
 * @output The composer drawer that allows or denies that one call
 * @position The answer surface for tools the session asks about before running
 *
 * Unlike the draft drawer there is nothing to edit: the call is fixed and the
 * endpoint refuses `update`, so the only answers are Allow and Deny. Deny needs a
 * reason because it goes back to the agent as the tool's result. The drawer is
 * not a modal, for the same reason as the draft one: the run waits, the page
 * does not.
 */
import {useEffect, useMemo, useState} from 'react';

import {Button, HStack, Text, TextArea, VStack} from '@astryxdesign/core';
import {CodeBlock} from '@astryxdesign/core/CodeBlock';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';

import type {WebUiInteraction} from '../api';
import {approvalRows, reviewApprovalAnswer, truncationNote, type ApprovalDecision} from '../approval-answer';
import {canExtendDraft, draftDeadlineLabel, draftExtendLabel} from '../draft-answer';

export interface ApprovalDrawerProps {
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
  /** The tallest a preview block grows before it scrolls. */
  previewMaxHeightPx: number;
  onAnswer: (body: {decision: ApprovalDecision; reason?: string}) => void;
  onExtend: () => void;
}

export function ApprovalDrawer({
  pending,
  isSubmitting,
  isExtending,
  error,
  reasonMaxLength,
  deadlineTickMs,
  previewMaxHeightPx,
  onAnswer,
  onExtend,
}: ApprovalDrawerProps) {
  const rows = useMemo(() => approvalRows(pending.draft), [pending.draft]);
  const [reason, setReason] = useState('');
  const [isDenying, setIsDenying] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), deadlineTickMs);
    return () => clearInterval(timer);
  }, [deadlineTickMs]);

  // A new question never inherits the last one's reason.
  useEffect(() => {
    setReason('');
    setIsDenying(false);
    setProblem(null);
  }, [pending.id]);

  const deadline = draftDeadlineLabel(pending.expires_at, now);
  const extendable = canExtendDraft(pending.expires_at, pending.max_expires_at, now);

  const submit = (decision: ApprovalDecision): void => {
    const reviewed = reviewApprovalAnswer({decision, reason, reasonMaxLength});
    if ('error' in reviewed) {
      setProblem(reviewed.error);
      return;
    }
    setProblem(null);
    onAnswer(reviewed.body);
  };

  return (
    <ChatComposerDrawer>
      <VStack gap={2} width="100%">
        <VStack gap={1} width="100%">
          <Text type="label">{pending.title ?? `Allow ${pending.tool}?`}</Text>
          <Text type="supporting" color="secondary">
            {`${pending.tool} will not run until you allow it${deadline === '' ? '' : ` — ${deadline}`}. Deny it with a reason and the agent sees the reason and tries something else. Typing below is not blocked.`}
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
        {rows.map((row) => (
          <VStack key={row.key} gap={1} width="100%">
            <Text type="supporting" color="secondary">
              {row.label}
            </Text>
            <CodeBlock
              code={row.text}
              language="text"
              size="sm"
              width="100%"
              container="card"
              maxHeight={previewMaxHeightPx}
              hasCopyButton
            />
            {row.truncatedBytes === undefined ? null : (
              <Text type="supporting" color="secondary">
                {truncationNote(row.truncatedBytes)}
              </Text>
            )}
          </VStack>
        ))}
        {isDenying ? (
          <TextArea
            label="Why are you denying it?"
            placeholder="Say what to do instead"
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
          <Button
            label="Allow"
            variant="primary"
            size="sm"
            isLoading={isSubmitting}
            isDisabled={isSubmitting}
            onClick={() => submit('accept')}
          />
          {isDenying ? (
            <Button
              label="Send denial"
              variant="destructive"
              size="sm"
              isDisabled={isSubmitting || reason.trim().length === 0}
              onClick={() => submit('decline')}
            />
          ) : (
            <Button
              label="Deny…"
              variant="ghost"
              size="sm"
              isDisabled={isSubmitting}
              onClick={() => setIsDenying(true)}
            />
          )}
        </HStack>
      </VStack>
    </ChatComposerDrawer>
  );
}
