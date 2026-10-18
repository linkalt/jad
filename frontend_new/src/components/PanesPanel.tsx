import {HStack, Spinner, Text, VStack} from '@astryxdesign/core';
import {List, ListItem} from '@astryxdesign/core/List';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';

import {paneCountSummary} from '../pane-count';
import {FloatingPanel} from './FloatingPanel';

/** One conversation on screen, as the panel lists it. */
export type PaneSummaryRow = {
  viewKey: string;
  /** Its place in the arrangement, counting from one, as the mobile bar numbers it. */
  number: number;
  /** Session · profile · provider · model · effort, the same line the pane strip shows. */
  title: string;
  isRunning: boolean;
  isFocused: boolean;
};

type PanesPanelProps = {
  rows: readonly PaneSummaryRow[];
  /** How many panes the configuration allows. */
  limit: number;
  region: ResizableRegion;
  onFocusPane: (viewKey: string) => void;
  onClose: () => void;
};

/**
 * The panes on screen, in the left floating window.
 *
 * This is where the desktop answers "how many are open": the footer beside its
 * icon carries the short count, and this panel repeats it with one row per pane —
 * its number, its title line, and a spinner while its run streams. A row focuses
 * that pane; closing stays on the pane's own ✕, so a row never holds a second
 * button inside a clickable one.
 */
export function PanesPanel({rows, limit, region, onFocusPane, onClose}: PanesPanelProps) {
  const running = rows.filter(row => row.isRunning).length;
  return (
    <FloatingPanel
      side="start"
      label="Panes"
      title="Panes"
      subtitle={paneCountSummary(rows.length, limit, running)}
      region={region}
      onClose={onClose}
    >
      <VStack gap={3} width="100%">
        <Text type="supporting" color="secondary">
          {rows.length === 1
            ? 'One conversation is on screen. Open another from a row\'s menu or a modifier-click.'
            : 'Choose a conversation to make it the one the composer belongs to.'}
        </Text>
        <List density="compact" hasDividers>
          {rows.map(row => (
            <ListItem
              key={row.viewKey}
              label={row.title}
              isSelected={row.isFocused}
              startContent={
                <HStack align="center" gap={1}>
                  <Text type="supporting" color="secondary">
                    {String(row.number)}
                  </Text>
                  {row.isRunning ? <Spinner size="sm" aria-label={`Pane ${row.number} running`} /> : null}
                </HStack>
              }
              onClick={() => onFocusPane(row.viewKey)}
            />
          ))}
        </List>
      </VStack>
    </FloatingPanel>
  );
}
