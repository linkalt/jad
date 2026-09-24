import type {WebUiMessageStep} from './api';

export function hiddenWebUiStepIds(
  steps: readonly WebUiMessageStep[] | undefined,
  visibleToolSteps: number,
): Set<string> {
  if (steps === undefined || steps.length === 0) {
    return new Set();
  }

  const requestedVisibleCount = Number.isSafeInteger(visibleToolSteps)
    ? Math.max(0, visibleToolSteps)
    : 0;
  const toolSteps = steps.filter(step => step.hasTools);
  const hasFinalAssistantStep = steps[steps.length - 1]?.hasTools === false;
  // If a run ended before its assistant summary, keep the latest tool step so
  // collapsing does not leave a blank assistant bubble.
  const fallbackVisibleCount = hasFinalAssistantStep ? 0 : Math.min(1, toolSteps.length);
  const visibleCount = Math.max(requestedVisibleCount, fallbackVisibleCount);
  const hiddenCount = Math.max(0, toolSteps.length - visibleCount);
  return new Set(toolSteps.slice(0, hiddenCount).map(step => step.id));
}

export function webUiStepAtPartIndex(
  steps: readonly WebUiMessageStep[] | undefined,
  partIndex: number,
): WebUiMessageStep | undefined {
  if (steps === undefined) {
    return undefined;
  }

  let low = 0;
  let high = steps.length - 1;
  while (low <= high) {
    const middle = low + Math.floor((high - low) / 2);
    const step = steps[middle];
    if (step === undefined) {
      return undefined;
    }
    if (partIndex < step.startPart) {
      high = middle - 1;
    } else if (partIndex >= step.endPart) {
      low = middle + 1;
    } else {
      return step;
    }
  }
  return undefined;
}

export function isWebUiPartHidden(
  steps: readonly WebUiMessageStep[] | undefined,
  hiddenStepIds: ReadonlySet<string>,
  partIndex: number,
): boolean {
  if (hiddenStepIds.size === 0) {
    return false;
  }
  const step = webUiStepAtPartIndex(steps, partIndex);
  return step !== undefined && hiddenStepIds.has(step.id);
}

export function visibleWebUiItems<T>(
  items: readonly T[],
  partIndex: (item: T) => number,
  steps: readonly WebUiMessageStep[] | undefined,
  hiddenStepIds: ReadonlySet<string>,
): T[] {
  return items.filter(
    item => !isWebUiPartHidden(steps, hiddenStepIds, partIndex(item)),
  );
}
