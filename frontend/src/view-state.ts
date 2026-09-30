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

/** Whether a session run should be attached for this view, not another view. */
export function canAttachRunToView<TController>(
  attachedTurnByView: ReadonlyMap<string, string>,
  controllersByView: ReadonlyMap<string, TController>,
  viewKey: string,
  turnId: string,
): boolean {
  return attachedTurnByView.get(viewKey) !== turnId && !controllersByView.has(viewKey);
}
