/**
 * @file ReadmePanel.tsx
 * @input The active session and profile
 * @output The right-edge panel that shows the README of the session's working directory
 * @position A right-edge panel; opens at start when show_readme_by_default is true
 *
 * A project without a README is normal, so it is an empty state, not an error.
 * A late answer for an earlier session must not overwrite a newer one.
 *
 * The README comes from the project, which may be a clone nobody has read. Its
 * images show their alt text instead of loading, so opening the panel makes no
 * request to a host it names, and a link that is not a web address does nothing
 * instead of navigating the app away.
 */
import {useCallback, useEffect, useRef, useState} from 'react';

import {Button, HStack, Icon, Text, VStack} from '@astryxdesign/core';
import {EmptyState} from '@astryxdesign/core/EmptyState';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';

import {fetchSessionReadme} from '../api';
import {isWebLink, type SessionReadme} from '../readme-view';
import {FloatingPanel} from './FloatingPanel';
import {MarkdownText} from './MarkdownText';

export interface ReadmePanelProps {
  sessionId: string;
  profile: string | undefined;
  region: ResizableRegion;
  onClose: () => void;
}

function ReadmeImage({alt}: {alt: string}) {
  return <Text type="supporting" color="secondary">{`[${alt}]`}</Text>;
}

const readmeComponents = {image: ReadmeImage};

function keepWebLinksOnly(href: string): false | undefined {
  return isWebLink(href) ? undefined : false;
}

export function ReadmePanel({sessionId, profile, region, onClose}: ReadmePanelProps) {
  const [readme, setReadme] = useState<SessionReadme | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [isLoading, setIsLoading] = useState(true);
  const loadSequence = useRef(0);

  const load = useCallback(() => {
    const sequence = ++loadSequence.current;
    setIsLoading(true);
    setLoadError(null);
    fetchSessionReadme(sessionId, profile)
      .then(result => {
        if (sequence === loadSequence.current) {
          setReadme(result);
        }
      })
      .catch((error: unknown) => {
        if (sequence === loadSequence.current) {
          setLoadError(error instanceof Error ? error.message : 'request failed');
        }
      })
      .finally(() => {
        if (sequence === loadSequence.current) {
          setIsLoading(false);
        }
      });
  }, [sessionId, profile]);

  useEffect(() => {
    setReadme(null);
    load();
    return () => {
      loadSequence.current++;
    };
  }, [load]);

  return (
    <FloatingPanel
      side="end"
      label="README"
      title="README"
      subtitle={readme?.name ?? 'Project overview'}
      region={region}
      onClose={onClose}
    >
      <VStack gap={3} width="100%">
        <HStack align="center" justify="end" gap={2}>
          <Button label="Refresh" variant="ghost" size="sm" isLoading={isLoading} onClick={load} />
        </HStack>
        {loadError === null ? null : (
          <Text type="supporting" color="secondary">
            {loadError}
          </Text>
        )}
        {readme !== null ? (
          <>
            <MarkdownText
              density="compact"
              contentWidth="100%"
              headingLevelStart={3}
              components={readmeComponents}
              onLinkClick={keepWebLinksOnly}
            >
              {readme.content}
            </MarkdownText>
            {readme.truncated ? (
              <Text type="supporting" color="secondary">
                The file is longer than the server shows; the end is cut.
              </Text>
            ) : null}
          </>
        ) : !isLoading && loadError === null ? (
          <EmptyState
            title="No README"
            description="Put a README.md at the top of the session's working directory to see it here."
            icon={<Icon icon="app:readme" size="sm" />}
            isCompact
          />
        ) : null}
      </VStack>
    </FloatingPanel>
  );
}
