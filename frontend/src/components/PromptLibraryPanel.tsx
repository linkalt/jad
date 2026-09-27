import {useCallback, useEffect, useMemo, useRef, useState, type ReactNode, type SVGProps} from 'react';
import {
  Icon,
  IconButton,
  List,
  ListItem,
  SegmentedControl,
  SegmentedControlItem,
  StackItem,
  Text,
  TextInput,
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
import {filterLibraryItems} from '../library-search';
import {FloatingPanel} from './FloatingPanel';

type LibraryTab = 'prompts' | 'templates' | 'skills';

type PromptLibraryPanelProps = {
  region: ResizableRegion;
  onClose: () => void;
  onUsePrompt: (asset: RootLibraryAsset) => void;
  onUseTemplate: (asset: RootLibraryAsset) => void;
  onUseSkill: (asset: RootLibraryAsset) => void;
};

// The icon registry has no name for these four glyphs, and Astryx accepts a
// plain SVG component anywhere an icon name goes.

function ReloadIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5} {...props}>
      <path d="M20.5 12a8.5 8.5 0 1 1-2.5-6" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M20.5 3.5v5h-5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/** Saved instructions: the shell prompt a run is started from. */
function PromptIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5} {...props}>
      <path d="M5.75 7.75 9.5 11.5l-3.75 3.75" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M12.75 15.75h5.5" strokeLinecap="round" />
    </svg>
  );
}

/** A page with a folded corner: a reusable document body. */
function TemplateIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5} {...props}>
      <path
        d="M13.5 4.75H7.75a1.5 1.5 0 0 0-1.5 1.5v11.5a1.5 1.5 0 0 0 1.5 1.5h8.5a1.5 1.5 0 0 0 1.5-1.5V9z"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <path d="M13.5 4.75V9h4.25" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/** A four-point sparkle: instructions that change how a run behaves. */
function SkillIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5} {...props}>
      <path
        d="M12 4.5l1.75 4.75L18.5 11l-4.75 1.75L12 17.5l-1.75-4.75L5.5 11l4.75-1.75z"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

const LIBRARY_TABS: ReadonlyArray<{id: LibraryTab; label: string; icon: ReactNode}> = [
  {id: 'prompts', label: 'Prompts', icon: <Icon icon={PromptIcon} size="sm" />},
  {id: 'templates', label: 'Templates', icon: <Icon icon={TemplateIcon} size="sm" />},
  {id: 'skills', label: 'Skills', icon: <Icon icon={SkillIcon} size="sm" />},
];

/** setting.json key that feeds each type, quoted by the empty state. */
const LIBRARY_DIRECTORY_KEY: Record<LibraryTab, string> = {
  prompts: 'prompt_dirs',
  templates: 'template_dirs',
  skills: 'skill_dirs',
};

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
  const [query, setQuery] = useState('');
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

  const items = useMemo<RootLibraryAssetSummary[]>(() => catalog?.[tab] ?? [], [catalog, tab]);
  const visibleItems = useMemo(() => filterLibraryItems(items, query), [items, query]);
  const isSearching = query.trim().length > 0;
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
    : isSearching
      ? `${visibleItems.length} of ${items.length} ${tab}`
      : `${count} shared ${count === 1 ? 'item' : 'items'}`;

  return (
    <FloatingPanel
      side="start"
      label="Prompt library"
      title="Prompt library"
      subtitle={subtitle}
      region={region}
      onClose={onClose}
      footer={
        <>
          <IconButton
            label="Refresh library"
            icon={<Icon icon={ReloadIcon} size="sm" />}
            variant="ghost"
            size="sm"
            isLoading={isLoading}
            onClick={() => void refresh()}
          />
          <StackItem size="fill">
            <TextInput
              label="Search library"
              isLabelHidden
              placeholder={`Search ${tab}`}
              startIcon="search"
              hasClear
              size="sm"
              value={query}
              onChange={setQuery}
            />
          </StackItem>
          <SegmentedControl
            label="Library type"
            size="sm"
            value={tab}
            onChange={value => setTab(value as LibraryTab)}
          >
            {LIBRARY_TABS.map(option => (
              <SegmentedControlItem
                key={option.id}
                value={option.id}
                label={option.label}
                isLabelHidden
                icon={option.icon}
              />
            ))}
          </SegmentedControl>
        </>
      }
    >
      <VStack gap={4}>
        <Text type="supporting" color="secondary">
          Edit the Markdown files or directory arrays in the global setting.json; refresh to load saved changes.
        </Text>
        {error !== null ? (
          <Text type="supporting" color="secondary">{error}</Text>
        ) : null}
        {isLoading && catalog === null ? (
          <Text type="supporting" color="secondary">Loading shared library…</Text>
        ) : visibleItems.length === 0 ? (
          <Text type="supporting" color="secondary">
            {isSearching
              ? `No ${tab} match “${query.trim()}”.`
              : `No ${tab} found. Configure libraries.${LIBRARY_DIRECTORY_KEY[tab]} in the global setting.json.`}
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
            {visibleItems.map(item => (
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
