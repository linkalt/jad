import type {JsonRecord, SessionUsageSummary} from "../../persistence.js";
import {
  assistantTextDeltaFromEvent,
  assistantTextPartFromOutput,
} from "./assistant-output-web-ui.js";
import {
  appendTextPart,
  appendToolPart,
  assistantMessage,
  fieldsFromParts,
  isRecord,
  responseFromRecord,
  textFromParts,
  timestampFromRecord,
} from "./shared-web-ui.js";
import {
  reasoningDeltaFromEvent,
  reasoningPartFromOutput,
} from "./reasoning-web-ui.js";
import { sessionSettingsFromConfig } from "./setting-web-ui.js";
import {sessionUsageFromSummary} from "./usage-web-ui.js";
import type {WebUiMessageAppend} from "./incremental-web-ui.js";
import {
  toolCallArgumentsDeltaFromEvent,
  toolCallArgumentsFromEvent,
  toolCallPartFromAddedItem,
  toolCallPartFromAnyOutput,
  toolCallStreamTarget,
  toolResultPartFromOutput,
} from "./tool-read-web-ui.js";
import type {
  WebUiMessage,
  WebUiMessageStep,
  WebUiPart,
  WebUiSessionConfig,
  WebUiSessionSnapshot,
  WebUiToolCallPart,
} from "./types.js";

export function responseParts(value: JsonRecord): WebUiPart[] {
  const response = responseFromRecord(value);
  if (!Array.isArray(response.output)) {
    return [];
  }

  let parts: WebUiPart[] = [];
  for (const output of response.output) {
    if (!isRecord(output)) {
      continue;
    }

    const reasoning = reasoningPartFromOutput(output);
    if (reasoning) {
      parts = appendTextPart(parts, "reasoning", reasoning.text);
      continue;
    }

    const text = assistantTextPartFromOutput(output);
    if (text) {
      parts = appendTextPart(parts, "text", text.text);
      continue;
    }

    const call = toolCallPartFromAnyOutput(output, parts.length);
    if (call) {
      parts = appendToolPart(parts, call);
    }
  }
  return parts;
}

function mergeCompletedParts(
  streamedParts: WebUiPart[],
  completedParts: WebUiPart[],
  currentStepStart: number,
): WebUiPart[] {
  if (streamedParts.length === 0) {
    return completedParts;
  }
  let parts = streamedParts;
  for (const completedPart of completedParts) {
    if (completedPart.type === "tool_call" || completedPart.type === "tool_result") {
      const hasPart = parts.some(
        (part) =>
          part.type === completedPart.type && part.callId === completedPart.callId,
      );
      if (!hasPart) {
        parts = appendToolPart(parts, completedPart);
        continue;
      }
      if (completedPart.type === "tool_call") {
        // The provider's finished item is authoritative for the arguments, and
        // it also drops the in-progress marker of a streamed call.
        parts = parts.map((part): WebUiPart =>
          part.type === "tool_call" && part.callId === completedPart.callId
            ? {...completedPart}
            : part,
        );
      }
      continue;
    }
    if (!parts.slice(currentStepStart).some((part) => part.type === completedPart.type)) {
      parts = appendTextPart(parts, completedPart.type, completedPart.text);
    }
  }
  return parts;
}

function userMessage(value: JsonRecord, key: string): WebUiMessage | undefined {
  if (value.type !== "message" || value.role !== "user") {
    return undefined;
  }
  const text = textFromParts(value.content, ["input_text"]);
  if (text.length === 0) {
    return undefined;
  }
  return {
    id: `session-user-${key}`,
    sender: "user",
    text,
    timestamp: timestampFromRecord(value),
    parts: [{ type: "text", text }],
  };
}

/**
 * True for the record that opens a turn. A user message resets the reducer
 * state, which is what makes turn boundaries reusable between incremental
 * reads: everything before one is immutable.
 */
export function isUserTurnRecord(record: JsonRecord): boolean {
  return record.type === "message" && record.role === "user";
}

/**
 * Mutable reducer state for the transcript projection.
 *
 * Message indices are absolute, so a state captured at a turn boundary can be
 * resumed later with more records.
 */
export interface WebUiSessionState {
  messages: WebUiMessage[];
  toolMessages: Map<string, number>;
  activeToolMessageIndex: number | undefined;
  activeStepId: string | undefined;
}

export function createWebUiSessionState(): WebUiSessionState {
  return {
    messages: [],
    toolMessages: new Map(),
    activeToolMessageIndex: undefined,
    activeStepId: undefined,
  };
}

/** Copies the state so a cached view can be extended without being mutated. */
export function cloneWebUiSessionState(state: WebUiSessionState): WebUiSessionState {
  return {
    messages: [...state.messages],
    toolMessages: new Map(state.toolMessages),
    activeToolMessageIndex: state.activeToolMessageIndex,
    activeStepId: state.activeStepId,
  };
}

function providerStepId(record: JsonRecord, response: JsonRecord, key: string): string {
  if (typeof response.id === "string" && response.id.length > 0) {
    return response.id;
  }
  if (Number.isSafeInteger(record.session_step) && (record.session_step as number) > 0) {
    return `session-step-${record.session_step}`;
  }
  return `session-step-${key}`;
}

function appendMessageStep(
  message: WebUiMessage,
  previousStepId: string | undefined,
  stepId: string,
  partCount: number,
  hasTools: boolean,
): WebUiMessageStep[] {
  const startPart = message.parts.length;
  const previousSteps = message.steps === undefined
    ? [{
      id: previousStepId ?? `step-before-${stepId}`,
      startPart: 0,
      endPart: startPart,
      hasTools: true,
    }]
    : message.steps.map((step, index) => index === message.steps!.length - 1
      ? {...step, endPart: startPart}
      : step);
  return [
    ...previousSteps,
    {id: stepId, startPart, endPart: startPart + partCount, hasTools},
  ];
}

/**
 * Folds one transcript record into the projection.
 *
 * `key` identifies the record for synthetic message ids; callers pass the record
 * index, or the byte offset when reading the transcript incrementally, so ids
 * stay stable no matter which slice produced them.
 */
export function reduceWebUiRecord(
  state: WebUiSessionState,
  record: JsonRecord,
  key: string,
): void {
  const messages = state.messages;
  const user = userMessage(record, key);
  if (user) {
    state.toolMessages.clear();
    state.activeToolMessageIndex = undefined;
    state.activeStepId = undefined;
    messages.push(user);
    return;
  }

  const result = toolResultPartFromOutput(record, key);
  if (result) {
    const messageIndex = state.toolMessages.get(result.callId);
    if (messageIndex !== undefined) {
      const message = messages[messageIndex];
      if (message) {
        const parts = [...message.parts, result];
        const steps = message.steps?.map((step, index) =>
          index === message.steps!.length - 1 ? {...step, endPart: parts.length} : step,
        );
        messages[messageIndex] = {
          ...message,
          parts,
          ...(steps === undefined ? {} : {steps}),
          ...fieldsFromParts(parts),
        };
      }
    } else {
      messages.push(
        assistantMessage(
          `session-tool-${key}`,
          [result],
          timestampFromRecord(record),
          "complete",
        ),
      );
    }
    return;
  }

  if (record.type !== "response.completed") {
    return;
  }
  const response = responseFromRecord(record);
  const stepId = providerStepId(record, response, key);
  const parts = responseParts(record);
  if (parts.length === 0) {
    return;
  }

  const callParts = parts.filter(
    (part): part is WebUiToolCallPart => part.type === "tool_call",
  );
  if (callParts.length > 0) {
    let messageIndex = state.activeToolMessageIndex;
    const activeMessage =
      messageIndex === undefined ? undefined : messages[messageIndex];
    if (messageIndex === undefined || !activeMessage) {
      messageIndex = messages.length;
      messages.push(
        assistantMessage(
          typeof response.id === "string"
            ? response.id
            : `session-assistant-${key}`,
          parts,
          timestampFromRecord(response),
          "complete",
        ),
      );
    } else {
      const mergedParts = [...activeMessage.parts, ...parts];
      const steps = appendMessageStep(
        activeMessage,
        state.activeStepId,
        stepId,
        parts.length,
        true,
      );
      messages[messageIndex] = {
        ...activeMessage,
        parts: mergedParts,
        steps,
        ...fieldsFromParts(mergedParts),
      };
    }
    state.activeToolMessageIndex = messageIndex;
    state.activeStepId = stepId;
    for (const part of callParts) {
      state.toolMessages.set(part.callId, messageIndex);
    }
    return;
  }

  if (state.activeToolMessageIndex !== undefined) {
    const activeMessage = messages[state.activeToolMessageIndex];
    if (activeMessage?.parts.some((part) => part.type === "tool_call")) {
      const mergedParts = [...activeMessage.parts, ...parts];
      const steps = appendMessageStep(
        activeMessage,
        state.activeStepId,
        stepId,
        parts.length,
        false,
      );
      messages[state.activeToolMessageIndex] = {
        ...activeMessage,
        parts: mergedParts,
        steps,
        ...fieldsFromParts(mergedParts),
      };
      state.toolMessages.clear();
      state.activeToolMessageIndex = undefined;
      state.activeStepId = undefined;
      return;
    }
  }

  messages.push(
    assistantMessage(
      typeof response.id === "string"
        ? response.id
        : `session-assistant-${key}`,
      parts,
      timestampFromRecord(response),
      "complete",
    ),
  );
}

export function normalizeWebUiSession(
  id: string,
  config: WebUiSessionConfig,
  records: readonly JsonRecord[],
  usageSummary?: SessionUsageSummary,
): WebUiSessionSnapshot {
  const state = createWebUiSessionState();
  records.forEach((record, index) => {
    reduceWebUiRecord(state, record, String(index));
  });

  const usage = sessionUsageFromSummary(usageSummary);
  return {
    id,
    ...sessionSettingsFromConfig(config),
    turns: records.filter((record) => record.type === "response.completed").length,
    messages: state.messages,
    ...(usage === undefined ? {} : { usage }),
  };
}

export interface WebUiNormalizedStreamUpdate {
  message: WebUiMessage;
  append?: WebUiMessageAppend;
}

export class WebUiNormalizedRun {
  private parts: WebUiPart[] = [];
  private steps: WebUiMessageStep[] = [];
  private currentStepWasFallback = false;
  private readonly timestamp: string;
  // Stream events identify an item by `item_id` (and `output_index`), while a
  // tool part is keyed by its call id, so the two have to be correlated.
  private readonly callIdsByItemId = new Map<string, string>();
  private readonly callIdsByOutputIndex = new Map<number, string>();

  public constructor(private readonly messageId: string) {
    this.timestamp = new Date().toLocaleTimeString([], {
      hour: "numeric",
      minute: "2-digit",
    });
  }

  public update(event: JsonRecord): WebUiMessage | undefined {
    return this.updateForStream(event)?.message;
  }

  public updateForStream(event: JsonRecord): WebUiNormalizedStreamUpdate | undefined {
    if (event.type === "response.created") {
      const response = isRecord(event.response) ? event.response : undefined;
      this.ensureCurrentStep(typeof response?.id === "string" ? response.id : undefined);
      return undefined;
    }

    const textDelta = assistantTextDeltaFromEvent(event);
    if (textDelta !== undefined) {
      const previousPart = this.parts.at(-1);
      this.ensureCurrentStep();
      this.parts = appendTextPart(this.parts, "text", textDelta);
      this.updateCurrentStep();
      const append = previousPart?.type === "text" && textDelta.trim().length > 0
        ? {index: this.parts.length - 1, append: textDelta}
        : undefined;
      return this.streamingUpdate(append);
    }

    const reasoningDelta = reasoningDeltaFromEvent(event);
    if (reasoningDelta !== undefined) {
      const previousPart = this.parts.at(-1);
      this.ensureCurrentStep();
      this.parts = appendTextPart(this.parts, "reasoning", reasoningDelta);
      this.updateCurrentStep();
      const append = previousPart?.type === "reasoning" && reasoningDelta.trim().length > 0
        ? {index: this.parts.length - 1, append: reasoningDelta}
        : undefined;
      return this.streamingUpdate(append);
    }

    if (event.type === "response.output_item.added" && isRecord(event.item)) {
      this.ensureCurrentStep();
      const call = toolCallPartFromAddedItem(event.item, this.parts.length);
      if (call !== undefined) {
        this.rememberToolCall(call.callId, event, event.item);
        if (!this.hasToolCall(call.callId)) {
          this.parts = appendToolPart(this.parts, call);
          this.updateCurrentStep(true);
          return this.streamingUpdate();
        }
      }
      return undefined;
    }

    const argumentsDelta = toolCallArgumentsDeltaFromEvent(event);
    if (argumentsDelta !== undefined) {
      const callId = this.toolCallIdForEvent(event);
      if (callId !== undefined) {
        const index = this.parts.findIndex(
          (part) => part.type === "tool_call" && part.callId === callId,
        );
        const previousPart = this.parts[index];
        if (previousPart?.type === "tool_call") {
          const append = previousPart.partial === true && argumentsDelta.length > 0
            ? {index, append: argumentsDelta}
            : undefined;
          this.updateToolArguments(callId, (current) => `${current}${argumentsDelta}`, true);
          return this.streamingUpdate(append);
        }
      }
      return undefined;
    }

    const argumentsDone = toolCallArgumentsFromEvent(event);
    if (argumentsDone !== undefined) {
      const callId = this.toolCallIdForEvent(event);
      if (callId !== undefined && this.hasToolCall(callId)) {
        this.updateToolArguments(callId, () => argumentsDone, false);
        return this.streamingUpdate();
      }
      return undefined;
    }

    if (event.type === "response.output_item.done" && isRecord(event.item)) {
      this.ensureCurrentStep();
      const call = toolCallPartFromAnyOutput(event.item, this.parts.length);
      if (call !== undefined) {
        this.rememberToolCall(call.callId, event, event.item);
        if (this.hasToolCall(call.callId)) {
          // The finished item is authoritative: it replaces whatever arrived as
          // deltas and clears the partial marker.
          this.updateToolArguments(call.callId, () => call.arguments, false, call.name);
        } else {
          this.parts = appendToolPart(this.parts, call);
        }
        this.updateCurrentStep(true);
        return this.streamingUpdate();
      }
    }

    if (event.type === "response.completed") {
      const response = responseFromRecord(event);
      this.ensureCurrentStep(typeof response.id === "string" ? response.id : undefined);
      const completedParts = responseParts(event);
      const toolParts = completedParts.filter(
        (part): part is WebUiToolCallPart => part.type === "tool_call",
      );
      let appended = false;
      for (const part of toolParts) {
        if (this.hasToolCall(part.callId)) {
          continue;
        }
        this.parts = appendToolPart(this.parts, part);
        appended = true;
      }
      if (toolParts.length > 0) {
        this.updateCurrentStep(true);
      }
      if (appended) {
        return this.streamingUpdate();
      }
    }
    return undefined;
  }

  public toolResult(value: JsonRecord): WebUiMessage | undefined {
    const result = toolResultPartFromOutput(value, this.parts.length);
    if (!result) {
      return undefined;
    }
    this.ensureCurrentStep();
    this.parts = appendToolPart(this.parts, result);
    this.updateCurrentStep();
    return this.streamingMessage();
  }

  public complete(response: JsonRecord): WebUiMessage {
    this.ensureCurrentStep(typeof response.id === "string" ? response.id : undefined);
    const completedParts = responseParts(response);
    const currentStepStart = this.steps.at(-1)?.startPart ?? 0;
    this.parts = clearPartialToolCalls(
      mergeCompletedParts(this.parts, completedParts, currentStepStart),
    );
    this.updateCurrentStep(completedParts.some((part) => part.type === "tool_call"));
    return this.message("complete");
  }

  private ensureCurrentStep(id?: string): WebUiMessageStep {
    const current = this.steps.at(-1);
    if (current !== undefined && (id === undefined || current.id === id)) {
      return current;
    }
    if (current !== undefined && this.currentStepWasFallback && id !== undefined) {
      current.id = id;
      this.currentStepWasFallback = false;
      return current;
    }

    if (current !== undefined) {
      current.endPart = this.parts.length;
    }
    const step: WebUiMessageStep = {
      id: id ?? `${this.messageId}-step-${this.steps.length + 1}`,
      startPart: this.parts.length,
      endPart: this.parts.length,
      hasTools: false,
    };
    this.steps.push(step);
    this.currentStepWasFallback = id === undefined;
    return step;
  }

  private updateCurrentStep(hasTools = false): void {
    const step = this.ensureCurrentStep();
    step.endPart = this.parts.length;
    if (hasTools) {
      step.hasTools = true;
    }
  }

  private message(status: "streaming" | "complete"): WebUiMessage {
    const message = assistantMessage(this.messageId, this.parts, this.timestamp, status);
    const steps = this.steps
      .filter((step) => step.endPart > step.startPart)
      .map((step) => ({...step}));
    return steps.length > 1 ? {...message, steps} : message;
  }

  private hasToolCall(callId: string): boolean {
    return this.parts.some(
      (part) => part.type === "tool_call" && part.callId === callId,
    );
  }

  private rememberToolCall(callId: string, event: JsonRecord, item: JsonRecord): void {
    const itemId = typeof item.id === "string" && item.id.length > 0 ? item.id : undefined;
    if (itemId !== undefined) {
      this.callIdsByItemId.set(itemId, callId);
    }
    const {outputIndex} = toolCallStreamTarget(event);
    if (outputIndex !== undefined) {
      this.callIdsByOutputIndex.set(outputIndex, callId);
    }
  }

  private toolCallIdForEvent(event: JsonRecord): string | undefined {
    const {itemId, outputIndex} = toolCallStreamTarget(event);
    if (itemId !== undefined) {
      const known = this.callIdsByItemId.get(itemId);
      if (known !== undefined) {
        return known;
      }
    }
    if (outputIndex !== undefined) {
      return this.callIdsByOutputIndex.get(outputIndex);
    }
    return undefined;
  }

  private updateToolArguments(
    callId: string,
    argumentsText: (current: string) => string,
    partial: boolean,
    name?: string,
  ): void {
    this.parts = this.parts.map((part): WebUiPart => {
      if (part.type !== "tool_call" || part.callId !== callId) {
        return part;
      }
      const {partial: _streamed, ...rest} = part;
      const next: WebUiToolCallPart = {
        ...rest,
        arguments: argumentsText(part.arguments),
        ...(name === undefined ? {} : {name}),
      };
      return partial ? {...next, partial: true} : next;
    });
  }

  private streamingUpdate(append?: WebUiMessageAppend): WebUiNormalizedStreamUpdate {
    const message = this.streamingMessage();
    return append === undefined ? {message} : {message, append};
  }

  private streamingMessage(): WebUiMessage {
    return this.message("streaming");
  }
}

/** A finished message never carries the in-progress marker. */
function clearPartialToolCalls(parts: WebUiPart[]): WebUiPart[] {
  return parts.map((part): WebUiPart => {
    if (part.type !== "tool_call" || part.partial !== true) {
      return part;
    }
    const {partial: _streamed, ...rest} = part;
    return rest;
  });
}
