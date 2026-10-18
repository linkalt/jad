/** Update one view's state without changing any other view's entry. */
export function updateViewState<T>(
  state: Record<string, T>,
  viewKey: string,
  update: (current: T | undefined) => T | undefined,
): Record<string, T> {
  const current = state[viewKey];
  const next = update(current);
  if (next === current) {
    return state;
  }
  if (next === undefined) {
    if (!Object.prototype.hasOwnProperty.call(state, viewKey)) {
      return state;
    }
    const remaining = {...state};
    delete remaining[viewKey];
    return remaining;
  }
  return {...state, [viewKey]: next};
}

/**
 * Record the turn of a run this client owns (it started the run, or it is
 * attached to it) for one view.
 *
 * Both maps are updated together. `activeTurnByView` names the turn for
 * interaction and completion tracking; `attachedTurnByView` is what
 * `canAttachRunToView` consults. A poll taken in a run's first milliseconds can
 * still report it as running after the started-run stream has finished and
 * released its controller. With only the controller as a guard, that stale row
 * made the attach effect stream the run again and replay its own error.
 */
export function recordOwnedRunTurn(
  activeTurnByView: Map<string, string>,
  attachedTurnByView: Map<string, string>,
  viewKey: string,
  turnId: string,
): void {
  activeTurnByView.set(viewKey, turnId);
  attachedTurnByView.set(viewKey, turnId);
}

/** Whether a session run should be attached for this view, not another view. */
export function canAttachRunToView<TController>(
  attachedTurnByView: ReadonlyMap<string, string>,
  controllersByView: ReadonlyMap<string, TController>,
  viewKey: string,
  turnId: string,
): boolean {
  return attachedTurnByView.get(viewKey) !== turnId && !controllersByView.has(viewKey);
}
