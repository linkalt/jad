import {type ChatToolCallItem} from '@astryxdesign/core';
import {type SessionSnapshot, type WebUiInteraction, type WebUiMessage, type WebUiPart} from './api';
import {type ProviderRetryWait} from './provider-retry-view';
import {type SessionUiState} from './session-run-status';

type SessionState = SessionUiState;

export interface RunInteractionState {
  viewKey: string;
  turnId: string;
  pending: WebUiInteraction | undefined;
  received: boolean;
}

export interface RunRetryState {
  turnId: string;
  wait: ProviderRetryWait;
}

export interface InteractionUiState {
  answeringInteractionId?: string;
  extendingInteractionId?: string;
  error?: string;
}

export type TraceState = 'complete' | 'running' | 'pending';

export interface ModelOption {
  id: string;
  provider?: string;
  efforts?: string[];
  defaultEffort?: string;
}

export interface Session {
  id: string;
  title: string;
  time: string;
  turns: number;
  profileTurns?: Record<string, number>;
  state: SessionState;
  sessionRef?: string;
  rootLabel?: string;
  /** Groups sessions in the list; absent when the session has none. */
  category?: string;
  activeProfile?: string;
  /** The session's stored default profile by id; absent when the server names none. */
  defaultProfile?: string;
  profiles?: string[];
  profileNames?: Record<string, string>;
  modelDefault?: string;
  /** Why the configured model cannot be used; set, the user must choose one before a run. */
  modelFault?: string;
  /** The profile file's entity tag when the record was read; sent back with a model-default write. */
  profileEtag?: string;
  models?: string[];
  modelOptions?: ModelOption[];
  frontCompaction?: SessionSnapshot['front_compaction'];
  model?: {
    provider?: string;
    model?: string;
  };
  usage?: SessionSnapshot['usage'];
  /** The run the server is executing for this session, when there is one. */
  run?: SessionSnapshot['run'];
  /** The run of each path profile that has one, keyed by profile name. */
  runsByProfile?: SessionSnapshot['runs_by_profile'];
  /**
   * The profile this row is pinned to. A row without one is the session's own
   * row and follows the stored default; a pinned row is a session of its own
   * that shares the session's `setting.json`.
   */
  profile?: string;
}

export interface ReasoningStep {
  label: string;
  detail: string;
  status: TraceState;
}

export type MessageRecord = Omit<WebUiMessage, 'parts'> & {
  /** Data URLs of the images this tab just sent; the server's copy replaces them on the next load. */
  sentImages?: string[];
  /** Names of the files this tab just sent; the server's list replaces them after a reload. */
  sentFiles?: string[];
  parts?: WebUiPart[];
  reasoning?: ReasoningStep[];
  tools?: ChatToolCallItem[];
};
