import {Spinner, Text, VStack} from '@astryxdesign/core';

import {foldedPaneTitle} from '../pane-fold';

type FoldedPaneProps = {
  /** Its place in the arrangement, the number the mobile bar and the panel use. */
  number: number;
  label: string;
  isRunning: boolean;
};

/**
 * A pane squeezed below a usable width.
 *
 * Panes are flex columns: widening one takes room from the others, and the last
 * pane used to shrink to a sliver — 1px wide and off-screen once three panes
 * held the row — where focusing it left a composer nobody could see. Folding
 * keeps the pane on screen and useful at a glance: its number, whether its run
 * is streaming, and its label printed down the column. It deliberately holds no
 * composer, so no keystroke can disappear into it; giving it room again is a
 * divider drag away, and the left window's Panes panel focuses and scrolls to it.
 */
export function FoldedPane({number, label, isRunning}: FoldedPaneProps) {
  return (
    <VStack
      align="center"
      gap={1}
      height="100%"
      width="100%"
      paddingBlock={2}
      role="group"
      aria-label={foldedPaneTitle(number, label)}
      data-pane-folded=""
    >
      <Text type="supporting" color="secondary">
        {String(number)}
      </Text>
      {isRunning ? <Spinner size="sm" aria-label={`Pane ${number} running`} /> : null}
      {/* Printed down the column: a folded pane is wide enough for one line of
          text, not for a wrapped one. */}
      <Text
        type="supporting"
        color="secondary"
        style={{writingMode: 'vertical-rl', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap'}}
      >
        {label}
      </Text>
    </VStack>
  );
}
