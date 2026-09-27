import {useCallback, useEffect, useRef, useState} from 'react';
import {
  Button,
  HStack,
  List,
  ListItem,
  Text,
  VStack,
} from '@astryxdesign/core';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';
import {
  fetchRootLibrary,
  fetchRootLibraryAsset,
  type RootLibraryAsset,
  type RootLibraryAssetSummary,
  type RootLibraryCatalog,
} from '../api';
import {FloatingPanel} from './FloatingPanel';

type LibraryTab = 'prompts' | 'templates' | 'skills';

type PromptLibraryPanelProps = {
  region: ResizableRegion;
  onClose: () => void;
  onUsePrompt: (asset: RootLibraryAsset) => void;
  onUseTemplate: (asset: RootLibraryAsset) => void;
  onUseSkill: (asset: RootLibraryAsset) => void;
};

const LIBRARY_TABS: ReadonlyArray<{id: LibraryTab; label: string}> = [
  {id: 'prompts', label: 'Prompts'},
  {id: 'templates', label: 'Templates'},
  {id: 'skills', label: 'Skills'},
];

/** Reads the shared, file-backed library; the root setting and Markdown are refreshed on demand. */
export function PromptLibraryPanel({
  region,
  onClose,
  onUsePrompt,
  onUseTemplate,
  onUseSkill,
}: PromptLibraryPanelProps) {
  const [catalog, setCatalog] = useState<RootLibraryCatalog | null>(null);
  const [tab, setTab] = useState<LibraryTab>('prompts');
  const [isLoading, setIsLoading] = useState(false);
  const [usingId, setUsingId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const requestRef = useRef<AbortController | null>(null);

  const refresh = useCallback(async () => {
    requestRef.current?.abort();
    const request = new AbortController();
    requestRef.current = request;
    setIsLoading(true);
    try {
      const next = await fetchRootLibrary(request.signal);
      if (!request.signal.aborted) {
        setCatalog(next);
        setError(null);
      }
    } catch (reason) {
      if (!request.signal.aborted) {
        setError(reason instanceof Error ? reason.message : String(reason));
      }
    } finally {
      if (!request.signal.aborted) setIsLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
    const refreshWhenVisible = () => {
      if (document.visibilityState === 'visible') void refresh();
    };
    window.addEventListener('focus', refreshWhenVisible);
    document.addEventListener('visibilitychange', refreshWhenVisible);
    return () => {
      requestRef.current?.abort();
      window.removeEventListener('focus', refreshWhenVisible);
      document.removeEventListener('visibilitychange', refreshWhenVisible);
    };
  }, [refresh]);

  const items: RootLibraryAssetSummary[] = catalog?.[tab] ?? [];
  const count = catalog === null
    ? 0
    : catalog.prompts.length + catalog.templates.length + catalog.skills.length;

  const useAsset = useCallback(async (summary: RootLibraryAssetSummary) => {
    setUsingId(summary.id);
    setError(null);
    try {
      // Detail is read separately so selecting an item always picks up the latest file contents.
      const asset = await fetchRootLibraryAsset(summary.kind, summary.id);
      if (asset.kind === 'prompt') onUsePrompt(asset);
      else if (asset.kind === 'template') onUseTemplate(asset);
      else onUseSkill(asset);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setUsingId(null);
    }
  }, [onUsePrompt, onUseSkill, onUseTemplate]);

  const subtitle = catalog === null
    ? 'Shared Markdown library'
    : `${count} shared ${count === 1 ? 'item' : 'items'}`;

  return (
    <FloatingPanel
      side="start"
      label="Prompt library"
      title="Prompt library"
      subtitle={subtitle}
      region={region}
      onClose={onClose}
    >
      <VStack gap={4}>
        <HStack align="center" justify="between" gap={2}>
          <HStack align="center" gap={1}>
            {LIBRARY_TABS.map(option => (
              <Button
                key={option.id}
                label={option.label}
                variant={tab === option.id ? 'secondary' : 'ghost'}
                size="sm"
                onClick={() => setTab(option.id)}
              />
            ))}
          </HStack>
          <Button
            label={isLoading ? 'Refreshing…' : 'Refresh'}
            variant="ghost"
            size="sm"
            isDisabled={isLoading}
            onClick={() => void refresh()}
          />
        </HStack>
        <Text type="supporting" color="secondary">
          Edit the Markdown files or directory arrays in the global setting.json; refresh to load saved changes.
        </Text>
        {error !== null ? (
          <Text type="supporting" color="secondary">{error}</Text>
        ) : null}
        {isLoading && catalog === null ? (
          <Text type="supporting" color="secondary">Loading shared library…</Text>
        ) : items.length === 0 ? (
          <Text type="supporting" color="secondary">
            No {tab} found. Configure libraries.{tab === 'prompts' ? 'prompt_dirs' : tab === 'templates' ? 'template_dirs' : 'skill_dirs'} in the global setting.json.
          </Text>
        ) : (
          <List
            hasDividers
            header={
              <Text type="supporting" color="secondary">
                {tab === 'templates'
                  ? 'Selecting a template inserts its full text into the composer.'
                  : tab === 'skills'
                    ? 'Selecting a skill adds its SKILL.md instructions as a badge.'
                    : 'Selecting a prompt inserts its text; long prompts use the composer badge threshold.'}
              </Text>
            }
          >
            {items.map(item => (
              <ListItem
                key={item.id}
                label={item.name}
                description={
                  <VStack gap={1}>
                    {item.description.length > 0 ? (
                      <Text type="supporting" color="secondary">{item.description}</Text>
                    ) : null}
                    <Text type="supporting" color="secondary">{item.source}</Text>
                  </VStack>
                }
                endContent={usingId === item.id ? <Text type="supporting">Loading…</Text> : undefined}
                onClick={() => void useAsset(item)}
              />
            ))}
          </List>
        )}
      </VStack>
    </FloatingPanel>
  );
}
