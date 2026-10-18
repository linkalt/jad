/**
 * The panels that belong to one conversation rather than to the app.
 *
 * The run inspector and the prompt library read one view's run, so each pane
 * owns them: opening the inspector in one pane leaves the others untouched.
 * The activity panels (notifications, checkpoints) are the app's own, and they
 * show in the left window — see `left-panel.ts`.
 */
export type ConversationPanelId = 'inspector' | 'prompts';

/**
 * A pane's own right-edge toggle: it closes the panel it already shows, and any
 * other toggle hands the pane's edge over.
 */
export function conversationPanelAfterToggle(
  current: ConversationPanelId | null,
  target: ConversationPanelId,
): ConversationPanelId | null {
  return current === target ? null : target;
}
