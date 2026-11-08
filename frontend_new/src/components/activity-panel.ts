import {createContext} from 'react';
import type {WebUiHostedToolPart, WebUiPart} from '../api';
import type {ReasoningStep} from '../app-types';
import type {TruncatedSource} from './TranscriptParts';


/**
 * What the pane's right-edge panel shows for one reasoning or tool block. It
 * carries the block's data, not rendered output, so the panel renders it with
 * the same components as the transcript and follows the block while it streams.
 */
export type ActivityDetail =
  | {
      key: string;
      kind: 'reasoning';
      content: string;
      steps: ReasoningStep[];
      isStreaming: boolean;
    }
  | {
      key: string;
      kind: 'tool';
      name: string;
      target?: string;
      call: Extract<WebUiPart, {type: 'tool_call'}>;
      result?: Extract<WebUiPart, {type: 'tool_result'}>;
      source?: TruncatedSource;
    }
  | {
      /** Several calls of one tool group, in transcript order. */
      key: string;
      kind: 'tools';
      items: {
        call: Extract<WebUiPart, {type: 'tool_call'}>;
        result?: Extract<WebUiPart, {type: 'tool_result'}>;
        source?: TruncatedSource;
      }[];
    }
  | {
      key: string;
      kind: 'hosted';
      name: string;
      target?: string;
      part: WebUiHostedToolPart;
      source?: TruncatedSource;
    };

export type ActivityPanelControl = {
  /** Key of the block whose detail the panel shows, if it is open. */
  openKey: string | undefined;
  /** Open (or, for the open block, close) the panel; `opener` gets focus back on close. */
  toggle: (detail: ActivityDetail, opener: HTMLElement) => void;
  /** Refresh the panel's detail when the block it shows has changed. */
  update: (detail: ActivityDetail) => void;
};

/** Absent outside a pane: the blocks then render as plain rows that open nothing. */
export const ActivityPanelContext = createContext<ActivityPanelControl | null>(null);
