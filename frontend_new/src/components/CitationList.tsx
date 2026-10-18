/**
 * @file CitationList.tsx
 * @input The web sources the provider cited in one answer text
 * @output A short "Sources" label and one line per source, as a link when the URL is safe
 * @position Rendered under the Markdown of an answer text that carries citations
 *
 * A source whose URL is not http or https is shown as plain text, never as a link.
 */
import {Text, VStack} from '@astryxdesign/core';
import {Link} from '@astryxdesign/core/Link';

import type {WebUiCitation} from '../api';
import {citationLabel, safeCitationHref} from '../web-search-view';

export interface CitationListProps {
  citations: readonly WebUiCitation[];
}

export function CitationList({citations}: CitationListProps) {
  if (citations.length === 0) {
    return null;
  }
  return (
    <VStack gap={1} width="100%">
      <Text type="supporting" color="secondary">Sources</Text>
      {citations.map((citation, index) => {
        const href = safeCitationHref(citation.url);
        // The number is part of the text, not a flex sibling: a row would keep
        // the label at its full width and the one-line truncation would never apply.
        const label = `${index + 1}. ${citationLabel(citation)}`;
        return href === undefined ? (
          <Text key={`${index}-${citation.url}`} type="supporting" maxLines={1}>
            {label}
          </Text>
        ) : (
          <Link
            key={`${index}-${citation.url}`}
            href={href}
            isExternalLink
            size="sm"
            maxLines={1}
          >
            {label}
          </Link>
        );
      })}
    </VStack>
  );
}
