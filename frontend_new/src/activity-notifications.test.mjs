import assert from 'node:assert/strict';
import test from 'node:test';

import {
  hasUnreadActivityError,
  isRunOutcomeUnknown,
  markActivityNotificationsRead,
  prependActivityNotification,
  runOutcomeUnknownNotification,
  unreadActivityNotificationCount,
} from './activity-notifications.ts';

function notification(overrides = {}) {
  return {
    id: 'event-a',
    sessionId: 'session-a',
    sessionTitle: 'Session A',
    kind: 'info',
    title: 'History compacted',
    message: 'Retained 4 of 8 items.',
    createdAt: 100,
    isRead: false,
    ...overrides,
  };
}

test('an idle stream with no message is an unknown outcome, not a failure or a success', () => {
  assert.equal(isRunOutcomeUnknown({idle: true}), true);
  assert.equal(isRunOutcomeUnknown({idle: true, message: undefined}), true);
  // A message means the run finished and its answer arrived.
  assert.equal(isRunOutcomeUnknown({idle: true, message: {id: 'turn-1'}}), false);
  assert.equal(isRunOutcomeUnknown({message: {id: 'turn-1'}}), false);
  assert.equal(isRunOutcomeUnknown({}), false);
  assert.equal(isRunOutcomeUnknown({idle: false}), false);
});

test('the unknown-outcome notice is informational and names the session', () => {
  const notice = runOutcomeUnknownNotification('session-a', 'Session A');

  assert.equal(notice.sessionId, 'session-a');
  assert.equal(notice.sessionTitle, 'Session A');
  // Not an error: the run may have succeeded, and an error would also light the
  // unread-error highlight for something that is not known to have failed.
  assert.equal(notice.kind, 'info');
  assert.equal(hasUnreadActivityError([{...notice, id: 'n', createdAt: 1, isRead: false}]), false);
  assert.match(notice.message, /transcript/);
});

test('prepends new notifications and respects the configured history limit', () => {
  const first = notification();
  const second = notification({id: 'event-b', createdAt: 200});
  const third = notification({id: 'event-c', createdAt: 300});
  const existing = [second, first];

  const updated = prependActivityNotification(existing, third, 2);

  assert.deepEqual(updated, [third, second]);
  assert.deepEqual(existing, [second, first]);
});

test('opening activity marks unread notifications read without replacing an unchanged list', () => {
  const read = notification({id: 'event-read', isRead: true});
  const unread = notification({id: 'event-unread'});
  const events = [read, unread];
  const updated = markActivityNotificationsRead(events);
  const alreadyRead = [read];

  assert.deepEqual(updated, [read, {...unread, isRead: true}]);
  assert.equal(updated[0], read);
  assert.equal(unread.isRead, false);
  assert.equal(markActivityNotificationsRead(alreadyRead), alreadyRead);
});

test('counts unread notifications and highlights unread errors', () => {
  const events = [
    notification({id: 'info-unread'}),
    notification({id: 'error-unread', kind: 'error'}),
    notification({id: 'error-read', kind: 'error', isRead: true}),
  ];

  assert.equal(unreadActivityNotificationCount(events), 2);
  assert.equal(hasUnreadActivityError(events), true);
  assert.equal(hasUnreadActivityError(events.slice(0, 1)), false);
});
