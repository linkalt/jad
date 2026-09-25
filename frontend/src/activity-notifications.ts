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
