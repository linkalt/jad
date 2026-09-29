/**
 * @file interactions.ts
 * @input A tool's question, the reply the answer endpoint received
 * @output Exports InteractionBroker and the reply parser
 * @position Between a tool that must ask the user and the run's message state
 *
 * A tool cannot answer itself and cannot read the run stream, so the question
 * waits here: `begin` hands it to the run — which publishes it as message state
 * — and returns a promise that the answer endpoint, the deadline, or the run's
 * abort signal settles. The tool result then carries that outcome back to the
 * model, so a declined draft is an ordinary turn step instead of a failed call.
 *
 * One question is pending at a time: the runner executes a turn's tool calls in
 * order, so a second `begin` before the first settles is a bug, not a queue.
 *
 * The deadline is not fixed: `extend` pushes it out by the question's own step,
 * up to the ceiling the session set, and republishes the question so a client
 * that is watching sees the new time left without asking again.
 */
import {randomUUID} from "node:crypto";

import {draftBoundsError, type DraftBounds} from "./json-bounds.js";
import type {JsonRecord} from "./persistence.js";

export type InteractionDecision = "accept" | "update" | "decline";

/** Why the wait ended: the user answered, the deadline passed, or the run stopped. */
export type InteractionOutcome = "answered" | "timed_out" | "cancelled";

/** What a tool asks, before the run attaches the identity of the call. */
export interface InteractionQuestion {
  /** The dynamic JSON the tool proposes and the user decides on. */
  draft: JsonRecord;
  title?: string;
  /** How long to wait for the user; the tool's configured policy. */
  timeout_ms: number;
  /** How much one extension adds to the deadline; zero when extending is off. */
  extend_ms?: number;
  /** The ceiling for the whole wait, extensions included. */
  max_timeout_ms?: number;
  /**
   * Bounds the user's edit is re-checked against. They travel with the question
   * because they are the tool's policy, and the answer endpoint has no other way
   * to know how large this session lets a draft be.
   */
  limits?: DraftBounds;
}

export interface InteractionIdentity {
  tool: string;
  tool_call_id: string;
}

/**
 * The question as a client sees it. `expires_at` is published so a UI can tell
 * the user how long the run is willing to wait without guessing at a deadline.
 */
export interface PendingInteraction extends InteractionIdentity {
  id: string;
  /** The tool's proposal, exactly as the tool sent it. */
  draft: JsonRecord;
  title?: string;
  created_at: string;
  expires_at: string;
  /** The latest deadline an extension may push to; equal to `expires_at` when extending is off. */
  max_expires_at: string;
  /** How much one extension adds to the deadline; zero when the session offers no more time. */
  extend_ms: number;
}

export interface InteractionAnswer {
  decision: InteractionDecision;
  /** The accepted draft: the user's edited version when they changed it. */
  draft?: JsonRecord;
  reason?: string;
  outcome: InteractionOutcome;
}

export interface InteractionReply {
  decision: InteractionDecision;
  draft?: JsonRecord;
  reason?: string;
}

/** A reply with the question it answers, as the endpoint received it. */
export interface InteractionReplyRequest extends InteractionReply {
  interaction_id: string;
}

/** Why a reply was not applied: nothing is pending, or the deadline passed. */
export type InteractionStatus = "answered" | "unknown" | "expired";

/** Why an extension was not applied: nothing is pending, the wait is over, or the ceiling is reached. */
export type InteractionExtendStatus = "extended" | "unknown" | "expired" | "at_max";

export type InteractionListener = (pending: PendingInteraction | undefined) => void;

/** Decline reasons are prose for the model; anything longer is a paste, not a reason. */
export const INTERACTION_REASON_MAX_CHARS = 2000;

export class InteractionBroker {
  private entry:
    | {
        pending: PendingInteraction;
        settle: (answer: InteractionAnswer) => void;
        timer: NodeJS.Timeout;
        detach: () => void;
        limits?: DraftBounds;
      }
    | undefined;

  public constructor(private readonly onChange: InteractionListener) {}

  public get pending(): PendingInteraction | undefined {
    return this.entry?.pending;
  }

  /**
   * The bounds the pending question's edited draft is re-checked against. The
   * answer endpoint reads them here rather than from config, so the check follows
   * the question that is actually waiting.
   */
  public get limits(): DraftBounds | undefined {
    return this.entry?.limits;
  }

  /** Publishes the question and resolves when it is answered, abandoned or stopped. */
  public begin(
    question: InteractionQuestion,
    identity: InteractionIdentity,
    signal?: AbortSignal,
  ): Promise<InteractionAnswer> {
    if (this.entry !== undefined) {
      throw new Error("an interaction is already pending for this run");
    }
    const timeoutMs = Math.max(1, Math.floor(question.timeout_ms));
    const extendMs = Math.max(0, Math.floor(question.extend_ms ?? 0));
    const maxTimeoutMs = Math.max(timeoutMs, Math.floor(question.max_timeout_ms ?? timeoutMs));
    const createdAt = Date.now();
    const pending: PendingInteraction = {
      id: randomUUID(),
      tool: identity.tool,
      tool_call_id: identity.tool_call_id,
      draft: question.draft,
      ...(question.title === undefined ? {} : {title: question.title}),
      created_at: new Date(createdAt).toISOString(),
      expires_at: new Date(createdAt + timeoutMs).toISOString(),
      max_expires_at: new Date(createdAt + maxTimeoutMs).toISOString(),
      extend_ms: extendMs,
    };

    return new Promise<InteractionAnswer>((resolve) => {
      const settle = (answer: InteractionAnswer, notify = true): void => {
        const active = this.entry;
        if (active === undefined) {
          return;
        }
        this.entry = undefined;
        clearTimeout(active.timer);
        active.detach();
        // The question has to leave the run's state before the tool continues:
        // a client that re-attaches must not see a question it cannot answer.
        if (notify) {
          this.onChange(undefined);
        }
        resolve(answer);
      };
      // The timer reads the question as it stands when it fires, so an extension
      // that moved the deadline is what the timeout reason reports.
      const timer = setTimeout(() => {
        settle(timedOutAnswer(this.entry?.pending ?? pending));
      }, timeoutMs);
      timer.unref?.();
      const cancelled: InteractionAnswer = {
        decision: "decline",
        outcome: "cancelled",
        reason: "The run was stopped before the draft was answered.",
      };
      const onAbort = (): void => {
        settle(cancelled);
      };

      this.entry = {
        pending,
        settle,
        timer,
        ...(question.limits === undefined ? {} : {limits: question.limits}),
        detach: () => signal?.removeEventListener("abort", onAbort),
      };
      signal?.addEventListener("abort", onAbort, {once: true});
      if (signal?.aborted === true) {
        // A question nobody can answer is neither published nor withdrawn.
        settle(cancelled, false);
        return;
      }
      this.onChange(pending);
    });
  }

  /** Applies a reply to the pending question, if it is still the one waiting. */
  public reply(id: string, reply: InteractionReply): InteractionStatus {
    const entry = this.entry;
    if (entry === undefined || entry.pending.id !== id) {
      return "unknown";
    }
    if (Date.now() >= Date.parse(entry.pending.expires_at)) {
      // The timer is about to fire; settle here so the caller learns it is late
      // rather than that it answered.
      entry.settle(timedOutAnswer(entry.pending));
      return "expired";
    }
    entry.settle({
      decision: reply.decision,
      ...(reply.draft === undefined ? {} : {draft: reply.draft}),
      ...(reply.reason === undefined ? {} : {reason: reply.reason}),
      outcome: "answered",
    });
    return "answered";
  }

  /**
   * Pushes the deadline out by the question's own step, up to the ceiling the
   * session set for the whole wait. The timer moves with the deadline, and the
   * question is republished so a client that is watching shows the new time left
   * instead of the old deadline.
   */
  public extend(id: string): InteractionExtendStatus {
    const entry = this.entry;
    if (entry === undefined || entry.pending.id !== id) {
      return "unknown";
    }
    const now = Date.now();
    const expiresAt = Date.parse(entry.pending.expires_at);
    if (now >= expiresAt) {
      // The timer is about to fire; settle here rather than extend a dead wait.
      entry.settle(timedOutAnswer(entry.pending));
      return "expired";
    }
    const maxExpiresAt = Date.parse(entry.pending.max_expires_at);
    if (expiresAt >= maxExpiresAt) {
      return "at_max";
    }
    const nextExpiresAt = Math.min(expiresAt + entry.pending.extend_ms, maxExpiresAt);
    entry.pending = {...entry.pending, expires_at: new Date(nextExpiresAt).toISOString()};
    clearTimeout(entry.timer);
    const timer = setTimeout(() => entry.settle(timedOutAnswer(entry.pending)), nextExpiresAt - now);
    timer.unref?.();
    entry.timer = timer;
    this.onChange(entry.pending);
    return "extended";
  }

  /**
   * Ends a wait nobody can answer any more. The run calls this when it finishes
   * or is stopped, so the tool returns instead of holding the turn open.
   */
  public cancel(reason: string): void {
    this.entry?.settle({decision: "decline", outcome: "cancelled", reason});
  }
}

/** The answer a question settles with when its deadline passes, naming the whole wait. */
function timedOutAnswer(pending: PendingInteraction): InteractionAnswer {
  const waited = Date.parse(pending.expires_at) - Date.parse(pending.created_at);
  return {
    decision: "decline",
    outcome: "timed_out",
    reason: `No answer within ${formatWait(waited)}; the draft was not confirmed.`,
  };
}

/**
 * Validates what the extension endpoint received: it names the question to move.
 * There is no amount in the body by design — the step and the ceiling belong to
 * the session, not to the caller.
 */
export function parseInteractionExtendRequest(
  value: unknown,
): {interaction_id: string} | {error: string} {
  const record = asJsonRecord(value);
  if (record === undefined) {
    return {error: "body must be a JSON object"};
  }
  const id = record.interaction_id;
  if (typeof id !== "string" || id.length === 0) {
    return {error: "interaction_id must be a non-empty string"};
  }
  return {interaction_id: id};
}

/**
 * Validates what the answer endpoint received.
 *
 * `update` carries the user's edited draft and `decline` carries the reason the
 * user gave — both are required, because a silent "no" leaves the model with
 * nothing to act on. An `accept` keeps the tool's own draft, so one sent
 * alongside it is ignored rather than merged. When the question's limits are
 * known, the edited draft is measured against them here, so an edit cannot be
 * larger or deeper than the tool could have proposed.
 */
export function parseInteractionReplyRequest(
  value: unknown,
  limits?: DraftBounds,
): InteractionReplyRequest | {error: string} {
  const record = asJsonRecord(value);
  if (record === undefined) {
    return {error: "body must be a JSON object"};
  }
  const id = record.interaction_id;
  if (typeof id !== "string" || id.length === 0) {
    return {error: "interaction_id must be a non-empty string"};
  }
  const decision = record.decision;
  if (decision !== "accept" && decision !== "update" && decision !== "decline") {
    return {error: 'decision must be "accept", "update" or "decline"'};
  }
  const reason = record.reason;
  if (reason !== undefined && typeof reason !== "string") {
    return {error: "reason must be a string"};
  }
  const draft = record.draft;
  if (decision === "update") {
    const edited = asJsonRecord(draft);
    if (edited === undefined) {
      return {error: 'draft must be a JSON object when decision is "update"'};
    }
    if (limits !== undefined) {
      const objection = draftBoundsError(edited, limits);
      if (objection !== undefined) {
        return {error: `draft ${objection}`};
      }
    }
    return {
      ...(reason === undefined ? {} : {reason}),
      decision,
      draft: edited,
      interaction_id: id,
    };
  }
  if (decision === "decline") {
    const trimmed = typeof reason === "string" ? reason.trim() : "";
    if (trimmed.length === 0) {
      return {error: 'reason must say why when decision is "decline"'};
    }
    if (trimmed.length > INTERACTION_REASON_MAX_CHARS) {
      return {error: `reason must be at most ${INTERACTION_REASON_MAX_CHARS} characters`};
    }
    return {decision, reason: trimmed, interaction_id: id};
  }
  return {
    decision,
    ...(reason === undefined ? {} : {reason}),
    interaction_id: id,
  };
}

/** "5 minutes", "90 seconds" — the wait as the reason text spells it. */
export function formatWait(milliseconds: number): string {
  const seconds = Math.max(1, Math.round(milliseconds / 1000));
  if (seconds % 60 === 0) {
    const minutes = seconds / 60;
    return minutes === 1 ? "a minute" : `${minutes} minutes`;
  }
  return seconds === 1 ? "a second" : `${seconds} seconds`;
}

function asJsonRecord(value: unknown): JsonRecord | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as JsonRecord)
    : undefined;
}
