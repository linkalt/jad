import type {SessionStateContextResult} from './api.ts';

export type StateContextPreviewView =
  | {kind: 'empty'}
  | {kind: 'block'; text: string; summary: string | null};

/** A preview that carries text is shown whether or not it was stored; a result without text has nothing to show. */
export function stateContextPreviewView(result: SessionStateContextResult): StateContextPreviewView {
  if (result.text === undefined) {
    return {kind: 'empty'};
  }
  const summary =
    result.bytes === undefined
      ? null
      : result.role === undefined
        ? `${result.bytes} bytes`
        : `${result.bytes} bytes, role ${result.role}`;
  return {kind: 'block', text: result.text, summary};
}
