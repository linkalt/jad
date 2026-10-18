export type ActivityNotificationKind = 'info' | 'error';

export interface ActivityNotification {
  id: string;
  sessionId: string;
  sessionTitle: string;
  kind: ActivityNotificationKind;
  title: string;
  message: string;
  createdAt: number;
  isRead: boolean;
}

export type NewActivityNotification = Omit<ActivityNotification, 'id' | 'createdAt' | 'isRead'>;

/**
 * The notice for a run whose live connection dropped and whose retained server
 * state expired before the client reconnected. Nothing persists a failed run in
 * the transcript, so the client cannot tell success from failure here and must
 * say so rather than report either.
 */
export function runOutcomeUnknownNotification(
  sessionId: string,
  sessionTitle: string,
): NewActivityNotification {
  return {
    sessionId,
    sessionTitle,
    kind: 'info',
    title: 'Run outcome unknown',
    message:
      'The live connection to this run was lost and the server no longer holds it. ' +
      'Check the transcript for the result; a failed run leaves no record there.',
  };
}

/** Whether a finished stream ended without the server holding the run. */
export function isRunOutcomeUnknown(result: {idle?: boolean; message?: unknown}): boolean {
  return result.idle === true && result.message === undefined;
}

export function prependActivityNotification(
  notifications: ActivityNotification[],
  notification: ActivityNotification,
  historyLimit: number,
): ActivityNotification[] {
  return [notification, ...notifications].slice(0, Math.max(0, historyLimit));
}

export function markActivityNotificationsRead(
  notifications: ActivityNotification[],
): ActivityNotification[] {
  let hasUnread = false;
  const marked = notifications.map(notification => {
    if (notification.isRead) {
      return notification;
    }
    hasUnread = true;
    return {...notification, isRead: true};
  });
  return hasUnread ? marked : notifications;
}

export function unreadActivityNotificationCount(
  notifications: ActivityNotification[],
): number {
  return notifications.filter(notification => !notification.isRead).length;
}

export function hasUnreadActivityError(notifications: ActivityNotification[]): boolean {
  return notifications.some(notification => !notification.isRead && notification.kind === 'error');
}
