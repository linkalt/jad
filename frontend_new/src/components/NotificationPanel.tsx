import {Button, HStack, Icon, Text, VStack} from '@astryxdesign/core';
import {EmptyState} from '@astryxdesign/core/EmptyState';
import {List, ListItem} from '@astryxdesign/core/List';
import {FloatingPanel} from './FloatingPanel';
import type {ActivityNotification} from '../activity-notifications';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';

type NotificationPanelProps = {
  notifications: ActivityNotification[];
  region: ResizableRegion;
  onClose: () => void;
  onClear: () => void;
  onSelectSession: (sessionId: string) => void;
};

function notificationTime(timestamp: number): string {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'short',
    timeStyle: 'short',
  }).format(timestamp);
}

export function NotificationPanel({
  notifications,
  region,
  onClose,
  onClear,
  onSelectSession,
}: NotificationPanelProps) {
  return (
    <FloatingPanel
      side="start"
      label="Notifications"
      title="Notifications"
      subtitle="Recent session activity"
      region={region}
      onClose={onClose}
    >
      {notifications.length === 0 ? (
        <EmptyState
          title="No notifications yet"
          description="Run failures and compaction results will appear here."
          icon={<Icon icon="info" size="sm" />}
          isCompact
        />
      ) : (
        <VStack gap={3} width="100%">
          <HStack align="center" justify="between" gap={2}>
            <Text type="supporting" color="secondary">
              {notifications.length} {notifications.length === 1 ? 'event' : 'events'}
            </Text>
            <Button
              label="Clear history"
              variant="ghost"
              size="sm"
              onClick={onClear}
            />
          </HStack>
          <List header="Recent activity" density="compact" hasDividers>
            {notifications.map(notification => (
              <ListItem
                key={notification.id}
                label={notification.title}
                description={
                  <VStack gap={1}>
                    <Text type="supporting">{notification.message}</Text>
                    <Text type="supporting" color="secondary">
                      {notification.sessionTitle} · {notificationTime(notification.createdAt)}
                    </Text>
                  </VStack>
                }
                startContent={
                  <Icon
                    icon={notification.kind === 'error' ? 'error' : 'info'}
                    size="sm"
                  />
                }
                onClick={() => onSelectSession(notification.sessionId)}
              />
            ))}
          </List>
        </VStack>
      )}
    </FloatingPanel>
  );
}
