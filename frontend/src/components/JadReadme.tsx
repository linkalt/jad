/**
 * @file JadReadme.tsx
 * @input The bundled jad-readme.md
 * @output The conversation lane's startup view: how to use Jad
 * @position Replaces the transcript until a session or profile row is chosen
 */
import {VStack} from '@astryxdesign/core';

import readme from '../jad-readme.md?raw';
import {MarkdownText} from './MarkdownText';

export function JadReadme() {
  return (
    <VStack isScrollable width="100%" height="100%" padding={4}>
      <MarkdownText density="default" contentWidth="100%" headingLevelStart={1}>
        {readme}
      </MarkdownText>
    </VStack>
  );
}
