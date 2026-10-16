/**
 * @file IconGroup.tsx
 * A colored highlight around one run of related composer icon buttons. The
 * composer's icon row is grouped, and a group's tone is what tells one group
 * from another at a glance: a tinted, rounded surface with the matching border.
 * Every color, the radius and the border width come from Astryx tokens, so both
 * the light and the dark palette stay the theme's business, not this file's.
 */
import {HStack} from '@astryxdesign/core';
import {borderVars, colorVars, radiusVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';
import type {ReactNode} from 'react';

/** The palette pair a group is tinted with. One group, one tone. */
export type IconGroupTone = 'blue' | 'green' | 'purple';

const styles = stylex.create({
  group: {
    borderRadius: radiusVars['--radius-full'],
    borderStyle: 'solid',
    borderWidth: borderVars['--border-width'],
  },
  blue: {
    backgroundColor: colorVars['--color-background-blue'],
    borderColor: colorVars['--color-border-blue'],
  },
  green: {
    backgroundColor: colorVars['--color-background-green'],
    borderColor: colorVars['--color-border-green'],
  },
  purple: {
    backgroundColor: colorVars['--color-background-purple'],
    borderColor: colorVars['--color-border-purple'],
  },
});

export interface IconGroupProps {
  /** Names the group for assistive technology; the tone itself says nothing there. */
  label: string;
  tone: IconGroupTone;
  children: ReactNode;
}

/** A run of related icon buttons, highlighted so the group itself is visible. */
export function IconGroup({label, tone, children}: IconGroupProps) {
  return (
    <HStack
      align="center"
      gap={0}
      padding={0.5}
      role="group"
      aria-label={label}
      xstyle={[styles.group, styles[tone]]}
    >
      {children}
    </HStack>
  );
}
