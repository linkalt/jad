import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// In the collapsed rail a session row with profiles opens that profile list as a
// hover flyout, and SideNavItem renders it as a popover sibling of the row
// inside the row's own wrapper. The session's right-click menu lands in the same
// place, so with the flyout still up the two panels stack on one spot: the
// vertical menu appeared over the profile list (measured in the running app:
// both at x=48,y=41). The flyout cannot be relied on to dismiss itself, because
// the panel's own invoker is exempt from its light dismiss, so the session menu
// closes it explicitly. A profile row must not do this — its menu is rendered
// inside the flyout, which has to stay open for the menu to have an anchor.
const navigation = readFileSync(
  new URL('./components/SessionNavigation.tsx', import.meta.url),
  'utf8',
);

test('a session row right-click closes an open hover flyout', () => {
  assert.match(
    navigation,
    /function closeHoverFlyout\(anchor: HTMLElement\)/,
    'SessionNavigation must close the row flyout itself',
  );
  assert.match(
    navigation,
    /querySelector<HTMLElement>\('\[popover\]'\)/,
    'the flyout is the popover rendered beside the row in the row wrapper',
  );
  assert.match(navigation, /matches\(':popover-open'\)/, 'only an open flyout is touched');
  assert.match(navigation, /hidePopover\(\)/, 'the flyout is hidden, which Astryx syncs back into its state');
});

test('only a session row closes its flyout, never a profile row', () => {
  assert.match(
    navigation,
    /if \(target\.kind === 'session'\) \{\s*closeHoverFlyout\(anchor\);/,
    'the close must be gated on a session target',
  );
});
