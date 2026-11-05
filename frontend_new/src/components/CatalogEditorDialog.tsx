/**
 * @file CatalogEditorDialog.tsx
 * @input Whether the dialog is open, and a callback to close it
 * @output A resizable modal that lists the profile, model, session, toolset or MCP server files and edits one as raw JSON, or deletes one
 * @position The management surface for <.jad>/{profiles,models,tools,mcp} and the session files; every write goes through the catalog routes
 *
 * The text is checked against the kind's scheme as you type; the server's loader
 * has the last word, and its message is shown verbatim when it refuses a write.
 * A save carries the version the file had when it was opened, so an edit made
 * elsewhere in the meantime is refused instead of overwritten.
 *
 * Both axes are the user's, from any edge or corner: the body is dragged wider and
 * taller, each bounded by what the centred, CSS-clamped surface can actually paint.
 */
import {useCallback, useEffect, useId, useMemo, useRef, useState} from 'react';

import {Banner, Button, Dialog, HStack, Icon, IconButton, Layout, LayoutContent, LayoutFooter, StackItem, Text, TextInput, VStack} from '@astryxdesign/core';
import {ResizeHandle, useResizable, type ResizableProps, type ResizableRegion} from '@astryxdesign/core/Resizable';
import {pixel} from '@astryxdesign/core/Resizable/utils';
import {Divider} from '@astryxdesign/core/Divider';
import {Item} from '@astryxdesign/core/Item';
import {Breadcrumbs, BreadcrumbItem} from '@astryxdesign/core/Breadcrumbs';
import {Tab, TabList} from '@astryxdesign/core/TabList';
import * as stylex from '@stylexjs/stylex';
import {colorVars, fontWeightVars} from '@astryxdesign/core/theme/tokens.stylex';

import {
  deleteCatalogFile,
  fetchCatalogFile,
  fetchCatalogFiles,
  saveCatalogFile,
  type CatalogFileInfo,
  type CatalogKind,
} from '../api';
import {FILTER_ALL, type SortEntry} from '../catalog-list-view';
import {bodyDefaultHeight, bodyHeightBound, centredEdgeDelta, dialogMaxHeight, dialogWidthBound, regionFloor} from '../catalog-layout';
import appConfig from '../config.json' with {type: 'json'};
import {createCatalogValidator} from '../catalog-validation';
import type {DocumentProblem} from '../json-document';
import {
  CATALOG_KIND_VIEWS,
  DISCARD_PROMPT,
  canSave as canSaveGate,
  deleteFailure,
  deletePrompt,
  hasUnsavedWork,
  newIdProblem,
  normalizeLineBreaks,
  saveFailure,
  sortedIds,
  type DeletePrompt,
} from '../catalog-editor-view';
import {CatalogCornerGrips} from './CatalogCornerGrips';
import {CatalogListPane, DEFAULT_CATALOG_SORT} from './CatalogListPane';
import {CatalogConfirmDialog} from './CatalogConfirmDialog';
import {CatalogSchemaTab} from './CatalogSchemaTab';
import {JsonCodeEditor} from './JsonCodeEditor';

const MENU_KINDS: readonly CatalogKind[] = ['sessions', 'profiles', 'models', 'toolsets', 'mcp'];

// Item's own selected fill (--color-accent-muted) equals the dialog surface in dark mode.
const menuStyles = stylex.create({
  selected: {
    backgroundColor: colorVars['--color-border-emphasized'],
    fontWeight: fontWeightVars['--font-weight-medium'],
  },
});

// A handle's overlay band is anchored to the inline-end and block-end edges, which is
// where the outward-growing edges already are. The other two edges re-anchor the same
// band: merged after the component's own styles, so these insets are the ones that
// land, and no second kind of grip has to be invented for two of the four edges.
const frameStyles = stylex.create({
  startEdge: {insetInlineEnd: 'auto', insetInlineStart: 0},
  topEdge: {bottom: 'auto', top: 0},
  // The surface is a frame, never a scroll container. The styled `dialog` resolves to
  // `overflow: auto` (measured: the same element with its classes stripped computes
  // `auto`; a bare <dialog> computes `visible`), so a single pixel of overflow paints
  // both bars — as it did in the reporter's Firefox 158 on a fractional 1429.58px
  // surface: `scrollWidth - clientWidth = 1`, `scrollHeight - clientHeight = 1`.
  // Nothing here scrolls as a whole: the file table and the JSON editor scroll inside
  // their own panes, which is where a bar belongs. `clip`, not `hidden`: hidden still
  // scrolls by script and by keyboard, and dragging is bounded by the window, so a
  // scrollbar here could only ever report the engine's rounding.
  surface: {overflow: 'clip'},
});

type EditorTab = 'editor' | 'schema';

/** One kind's file list as last fetched, or the reason it could not be. */
type CatalogList = {files: CatalogFileInfo[]; error: string | null};

const NO_FILES: CatalogFileInfo[] = [];

type Draft =
  | {mode: 'none'}
  | {mode: 'loading'; id: string}
  | {mode: 'existing'; id: string; etag: string; savedText: string}
  | {mode: 'new'; id: string};

type Pending =
  | {kind: 'switch'; target: string | 'new'}
  | {kind: 'change-kind'; target: CatalogKind}
  | {kind: 'back'}
  | {kind: 'close'}
  | {kind: 'delete'}
  /** A row deleted straight from the list; the editor is closed, so it carries its own version. */
  | {kind: 'row-delete'; id: string; etag: string}
  | null;

/** The ID a duplicate of `id` proposes: `<category>/<name>-copy`, or the ID with `-copy`. */
export function duplicateId(id: string): string {
  return `${id}${appConfig.catalog_duplicate_suffix}`;
}

type Notice = {kind: 'error' | 'success'; title: string; message: string};

const NO_PROBLEMS: DocumentProblem[] = [];

export interface CatalogEditorDialogProps {
  isOpen: boolean;
  onClose: () => void;
  /** Called after a file is created, replaced or deleted, so the app re-reads session data. */
  onChanged: () => void;
}

export function CatalogEditorDialog({isOpen, onClose, onChanged}: CatalogEditorDialogProps) {
  const [kind, setKind] = useState<CatalogKind>('profiles');
  const [lists, setLists] = useState<Partial<Record<CatalogKind, CatalogList>>>({});
  const [draft, setDraft] = useState<Draft>({mode: 'none'});
  const [text, setText] = useState('');
  const [newId, setNewId] = useState('');
  const [checked, setChecked] = useState<{text: string; problems: DocumentProblem[]}>({text: '', problems: []});
  const [notice, setNotice] = useState<Notice | null>(null);
  const [checkError, setCheckError] = useState<string | null>(null);
  const [isBusy, setIsBusy] = useState(false);
  const [pending, setPending] = useState<Pending>(null);
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState(FILTER_ALL);
  const [sort, setSort] = useState<SortEntry[]>(DEFAULT_CATALOG_SORT);
  const [columnWidths, setColumnWidths] = useState<Record<string, number>>({});
  const [tab, setTab] = useState<EditorTab>('editor');
  const panelBaseId = useId();
  const panelId = (value: EditorTab) => `${panelBaseId}-${value}`;
  const validatorRef = useRef<ReturnType<typeof createCatalogValidator> | null>(null);
  const loadToken = useRef(0);

  // Both axes are draggable, and both are bounded by what the surface can paint
  // rather than by the configured numbers alone: `Dialog` clamps its own box, so a
  // region allowed past the clamp keeps a size nothing can show — measured, a 200 px
  // width drag on a 1238 px window left 1206 painted and 1240 stored, and the next
  // drag spent its first 34 px on the invisible excess. The viewport is tracked, not
  // read once, so a window that shrinks pulls both stored sizes down with it.
  const [viewport, setViewport] = useState(() => ({width: window.innerWidth, height: window.innerHeight}));
  useEffect(() => {
    const onResize = (): void => setViewport({width: window.innerWidth, height: window.innerHeight});
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);
  const widthBound = dialogWidthBound(
    viewport.width,
    appConfig.catalog_dialog_viewport_margin_px,
    appConfig.catalog_dialog_max_width_px,
  );
  const heightBound = bodyHeightBound(
    viewport.height,
    appConfig.catalog_dialog_max_height_percent,
    appConfig.catalog_dialog_chrome_height_px,
  );
  // A window narrower or shorter than the configured floor cannot paint that floor, so
  // the floor follows the bound down: a minimum above its own maximum would leave the
  // region claiming a size the surface does not have.
  const widthFloor = regionFloor(appConfig.catalog_dialog_min_width_px, widthBound);
  const heightFloor = regionFloor(appConfig.catalog_body_min_height_px, heightBound);

  // Each size is kept in localStorage under its own key, so both survive the close
  // that unmounts this component and a page reload. The reset-on-close effect below
  // deliberately does not touch them: that clears what the list was showing, not a
  // layout choice the user made.
  const widthRegion = useResizable({
    defaultSize: pixel(appConfig.catalog_dialog_width_px),
    minSize: pixel(widthFloor),
    maxSize: pixel(widthBound),
    direction: 'horizontal',
    autoSaveId: 'jad-catalog-dialog-width',
  });
  const heightRegion = useResizable({
    defaultSize: pixel(bodyDefaultHeight(viewport.height, appConfig.catalog_body_height_px, appConfig.catalog_body_viewport_percent)),
    minSize: pixel(heightFloor),
    maxSize: pixel(heightBound),
    direction: 'vertical',
    autoSaveId: 'jad-catalog-dialog-height',
  });

  const view = CATALOG_KIND_VIEWS[kind];
  // The dialog is centred on both axes, so the edge under the pointer travels half of
  // the size it gains. Without this both handles lag the cursor by 50 % for the whole
  // drag, which reads as a stuck handle rather than a heavy one. One helper, so the
  // two axes cannot drift apart.
  const centredProps = (region: ResizableRegion): ResizableProps => ({
    ...region.props,
    _onResizeMove: (delta: number) => region.props._onResizeMove(centredEdgeDelta(delta)),
  });
  const widthDragProps = centredProps(widthRegion);
  const heightDragProps = centredProps(heightRegion);

  const list = lists[kind];
  const files = list?.files ?? NO_FILES;
  const ids = useMemo(() => sortedIds(files.map(file => file.id)), [files]);
  const savedText = draft.mode === 'existing' ? draft.savedText : draft.mode === 'new' ? view.template : '';
  const unsaved = hasUnsavedWork(draft.mode, text, savedText, newId, view.template);
  const idProblem = draft.mode === 'new' ? newIdProblem(newId, ids, view.noun, view.idForm) : null;
  const isChecked = checked.text === text;
  // The editor keeps the last result while a new check is pending, so squiggles do not blink while typing.
  const problems = checked.problems;

  const loadList = useCallback(async (target: CatalogKind, signal?: AbortSignal): Promise<void> => {
    try {
      const next = await fetchCatalogFiles(target, signal);
      setLists(previous => ({...previous, [target]: {files: next, error: null}}));
    } catch (caught) {
      if (signal?.aborted) return;
      const message = caught instanceof Error ? caught.message : String(caught);
      setLists(previous => ({...previous, [target]: {files: previous[target]?.files ?? NO_FILES, error: message}}));
    }
  }, []);

  // A profile's Session column is read from the session files, so a session change refreshes profiles too.
  const reloadList = useCallback(async (): Promise<void> => {
    await Promise.all(kind === 'sessions' ? [loadList('sessions'), loadList('profiles')] : [loadList(kind)]);
  }, [kind, loadList]);

  // Every kind is fetched when the dialog opens, so the menu switches without waiting.
  useEffect(() => {
    if (!isOpen) return undefined;
    const controller = new AbortController();
    for (const target of MENU_KINDS) void loadList(target, controller.signal);
    return () => controller.abort();
  }, [isOpen, loadList]);

  useEffect(() => {
    if (!isOpen) {
      setKind('profiles');
      setLists({});
      setQuery('');
      setFilter(FILTER_ALL);
      setSort(DEFAULT_CATALOG_SORT);
      setColumnWidths({});
      setTab('editor');
      setDraft({mode: 'none'});
      setText('');
      setNotice(null);
      setPending(null);
      setNewId('');
      setCheckError(null);
      setChecked({text: '', problems: NO_PROBLEMS});
    }
  }, [isOpen]);

  // The schemes are 170 kB of JSON plus the validator, so they load with the first edit, not with the app.
  useEffect(() => {
    if (!isOpen || draft.mode === 'none' || draft.mode === 'loading') return undefined;
    let cancelled = false;
    const timer = window.setTimeout(() => {
      void (async () => {
        try {
          if (!validatorRef.current) {
            const {default: schemes} = await import('virtual:catalog-schemes');
            validatorRef.current = createCatalogValidator(schemes);
          }
          const result = validatorRef.current.validate(kind, text);
          if (!cancelled) {
            setCheckError(null);
            setChecked({text, problems: result});
          }
        } catch (caught) {
          if (!cancelled) setCheckError(caught instanceof Error ? caught.message : String(caught));
        }
      })();
    }, appConfig.catalog_lint_delay_ms);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [isOpen, kind, draft.mode, text]);

  const openExisting = useCallback(async (id: string): Promise<void> => {
    const token = ++loadToken.current;
    setDraft({mode: 'loading', id});
    setTab('editor');
    setNotice(null);
    try {
      const file = await fetchCatalogFile(kind, id);
      if (token !== loadToken.current) return;
      const loaded = normalizeLineBreaks(file.text);
      setText(loaded);
      setDraft({mode: 'existing', id, etag: file.etag, savedText: loaded});
    } catch (caught) {
      if (token !== loadToken.current) return;
      setDraft({mode: 'none'});
      setText('');
      setNotice({kind: 'error', title: `Could not open the ${view.noun}`, message: caught instanceof Error ? caught.message : String(caught)});
    }
  }, [kind, view.noun]);

  const startNew = useCallback((): void => {
    loadToken.current += 1;
    setNewId('');
    setText(view.template);
    setTab('editor');
    setNotice(null);
    setDraft({mode: 'new', id: ''});
  }, [view.template]);

  /**
   * Duplicate: a new file whose text is the open file's (as edited), with an ID proposed
   * from the open one that the user names before Create. Nothing is written until then.
   */
  const startDuplicate = (): void => {
    if (draft.mode !== 'existing') return;
    loadToken.current += 1;
    setNewId(duplicateId(draft.id));
    setTab('editor');
    setNotice(null);
    setDraft({mode: 'new', id: ''});
  };

  const changeKind = (target: CatalogKind): void => {
    loadToken.current += 1;
    setKind(target);
    setQuery('');
    setFilter(FILTER_ALL);
    setSort(DEFAULT_CATALOG_SORT);
    setColumnWidths({});
    setTab('editor');
    setDraft({mode: 'none'});
    setText('');
    setNewId('');
    setNotice(null);
    setCheckError(null);
    setChecked({text: '', problems: NO_PROBLEMS});
  };

  const backToList = (): void => {
    loadToken.current += 1;
    setDraft({mode: 'none'});
    setText('');
    setNewId('');
    setTab('editor');
    setNotice(null);
    setCheckError(null);
    setChecked({text: '', problems: NO_PROBLEMS});
  };

  const requestBack = (): void => {
    if (isBusy || draft.mode === 'none') return;
    if (unsaved) setPending({kind: 'back'});
    else backToList();
  };

  const requestKind = (target: CatalogKind): void => {
    if (target === kind) {
      requestBack();
      return;
    }
    if (unsaved) setPending({kind: 'change-kind', target});
    else changeKind(target);
  };

  const requestSwitch = (target: string | 'new'): void => {
    if (unsaved) {
      setPending({kind: 'switch', target});
      return;
    }
    if (target === 'new') startNew();
    else void openExisting(target);
  };

  const requestClose = (): void => {
    if (isBusy) return;
    if (unsaved) setPending({kind: 'close'});
    else onClose();
  };

  const confirmPending = (deleteState: boolean): void => {
    const action = pending;
    setPending(null);
    if (!action) return;
    if (action.kind === 'close') onClose();
    else if (action.kind === 'change-kind') changeKind(action.target);
    else if (action.kind === 'back') backToList();
    else if (action.kind === 'switch') {
      if (action.target === 'new') startNew();
      else void openExisting(action.target);
    } else if (action.kind === 'delete') void performDelete(deleteState);
    else if (action.kind === 'row-delete') void performRowDelete(action.id, action.etag, deleteState);
  };

  const save = async (): Promise<void> => {
    if (draft.mode !== 'existing' && draft.mode !== 'new') return;
    const isCreate = draft.mode === 'new';
    const id = isCreate ? newId.trim() : draft.id;
    setIsBusy(true);
    setNotice(null);
    try {
      const result = await saveCatalogFile(kind, id, text, isCreate ? null : draft.mode === 'existing' ? draft.etag : null);
      setDraft({mode: 'existing', id, etag: result.info.etag, savedText: text});
      setNotice({kind: 'success', title: 'Done', message: isCreate ? `Created ${id}.` : `Saved ${id}.`});
      await reloadList();
      onChanged();
    } catch (caught) {
      const failure = saveFailure(caught, isCreate);
      setNotice({kind: 'error', title: failure.kind === 'stale' ? 'Changed on the server' : 'Not saved', message: failure.message});
    } finally {
      setIsBusy(false);
    }
  };

  const performDelete = async (deleteState: boolean): Promise<void> => {
    if (draft.mode !== 'existing') return;
    setIsBusy(true);
    setNotice(null);
    try {
      await deleteCatalogFile(kind, draft.id, draft.etag, {deleteState});
      const removed = draft.id;
      setDraft({mode: 'none'});
      setText('');
      setNotice({kind: 'success', title: 'Done', message: `Deleted ${removed}.`});
      await reloadList();
      onChanged();
    } catch (caught) {
      setNotice({kind: 'error', title: 'Not deleted', message: deleteFailure(caught).message});
    } finally {
      setIsBusy(false);
    }
  };

  /**
   * Deletes one row of the list. The editor is closed in this mode, so the row
   * carries the version it was listed at. Only a session has a state folder, so
   * only a session's row asks about it, and the answer comes from the confirm.
   */
  const performRowDelete = async (id: string, etag: string, deleteState: boolean): Promise<void> => {
    setIsBusy(true);
    setNotice(null);
    try {
      await deleteCatalogFile(kind, id, etag, {deleteState});
      setNotice({kind: 'success', title: 'Done', message: `Deleted ${id}.`});
      await reloadList();
      onChanged();
    } catch (caught) {
      setNotice({kind: 'error', title: 'Not deleted', message: deleteFailure(caught).message});
    } finally {
      setIsBusy(false);
    }
  };

  const reloadCurrent = (): void => {
    if (draft.mode !== 'existing') return;
    if (unsaved) setPending({kind: 'switch', target: draft.id});
    else void openExisting(draft.id);
  };

  const hasEditor = draft.mode === 'existing' || draft.mode === 'new';
  const canSave = canSaveGate({
    mode: draft.mode,
    hasUnsavedWork: unsaved,
    isChecked: isChecked && checkError === null,
    problemCount: isChecked ? problems.length : 0,
    isBusy,
    idProblem,
  });
  const title = draft.mode === 'existing' ? `${view.noun}: ${draft.id}` : draft.mode === 'new' ? `New ${view.noun}` : view.plural;
  // One confirm asks every question. The wording is decided in one pure place and
  // re-reads as its checkbox is ticked, instead of branching four ways here.
  const isDelete = pending?.kind === 'delete' || pending?.kind === 'row-delete';
  const promptFor = (deleteState: boolean): DeletePrompt => {
    if (pending?.kind === 'delete') {
      return deletePrompt(kind, view, {id: draft.mode === 'existing' ? draft.id : null, deleteState});
    }
    if (pending?.kind === 'row-delete') return deletePrompt(kind, view, {id: pending.id, deleteState});
    return DISCARD_PROMPT;
  };

  return (
    <>
      <Dialog
        isOpen={isOpen}
        onOpenChange={open => {
          if (!open) requestClose();
        }}
        width={widthRegion.size}
        maxHeight={dialogMaxHeight(appConfig.catalog_dialog_max_height_percent)}
        xstyle={frameStyles.surface}
        purpose="form"
        aria-label={`Catalog: ${title}`}
      >
        <Layout
          content={
            <LayoutContent padding={0}>
              <HStack gap={3} align="stretch" padding={4} height={heightRegion.size}>
                <StackItem size="static">
                  <VStack gap={1} width={appConfig.catalog_file_list_width_px} role="navigation" aria-label="Catalog kinds">
                    {MENU_KINDS.map(option => (
                      <Item
                        key={option}
                        label={CATALOG_KIND_VIEWS[option].plural}
                        isSelected={option === kind}
                        xstyle={option === kind ? menuStyles.selected : undefined}
                        isDisabled={isBusy}
                        aria-current={option === kind ? 'page' : undefined}
                        onClick={() => requestKind(option)}
                      />
                    ))}
                  </VStack>
                </StackItem>
                <VStack><Divider orientation="vertical" variant="strong" /></VStack>
                <StackItem size="fill">
                <VStack gap={3} width="100%" height="100%">
                  {notice !== null && (
                    <Banner
                      status={notice.kind}
                      title={notice.title}
                      description={notice.message}
                      collapsible={false}
                      isDismissable
                      onDismiss={() => setNotice(null)}
                    />
                  )}
                  {draft.mode === 'none' && (
                    <CatalogListPane
                      kind={kind}
                      view={view}
                      files={files}
                      isLoading={list === undefined}
                      error={list?.error ?? null}
                      isDisabled={isBusy}
                      query={query}
                      filter={filter}
                      sort={sort}
                      columnWidths={columnWidths}
                      onSortChange={setSort}
                      onColumnWidthsChange={setColumnWidths}
                      onQueryChange={setQuery}
                      onFilterChange={setFilter}
                      onOpen={id => requestSwitch(id)}
                      onNew={() => requestSwitch('new')}
                      onRetry={() => void reloadList()}
                      onDelete={target => setPending({kind: 'row-delete', ...target})}
                    />
                  )}
                  {draft.mode !== 'none' && (
                    <HStack gap={2} align="center">
                      <IconButton
                        label={`Back to ${view.plural.toLowerCase()}`}
                        tooltip={`Back to ${view.plural.toLowerCase()}`}
                        icon={<Icon icon="chevronLeft" />}
                        variant="ghost"
                        isDisabled={isBusy}
                        onClick={requestBack}
                      />
                      <Breadcrumbs label="Catalog location">
                        <BreadcrumbItem onClick={requestBack}>{view.plural}</BreadcrumbItem>
                        <BreadcrumbItem isCurrent>{draft.mode === 'new' ? 'New' : draft.id}</BreadcrumbItem>
                      </Breadcrumbs>
                    </HStack>
                  )}
                  {draft.mode === 'loading' && <Text type="supporting" color="secondary">Loading {draft.id}…</Text>}
                  {draft.mode === 'new' && (
                    <TextInput
                      label="ID"
                      description={view.idHint}
                      value={newId}
                      onChange={setNewId}
                      hasAutoFocus
                      status={newId !== '' && idProblem !== null ? {type: 'error', message: idProblem} : undefined}
                    />
                  )}
                  {hasEditor && (
                    <TabList role="tablist" aria-label="Editor view" value={tab} onChange={value => setTab(value as EditorTab)} hasDivider>
                      <Tab value="editor" label="JSON" panelId={panelId('editor')} />
                      <Tab value="schema" label="Schema" panelId={panelId('schema')} />
                    </TabList>
                  )}
                  {hasEditor && tab === 'schema' && (
                    <StackItem size="fill" isScrollable id={panelId('schema')} role="tabpanel" aria-label="Schema">
                      <CatalogSchemaTab kind={kind} />
                    </StackItem>
                  )}
                  {hasEditor && tab === 'editor' && (
                    <>
                      <StackItem size="fill" id={panelId('editor')} role="tabpanel" aria-label="JSON">
                        <JsonCodeEditor
                          label={draft.mode === 'new' ? `New ${view.noun} JSON` : `${draft.id} JSON`}
                          value={text}
                          problems={problems}
                          isReadOnly={isBusy}
                          height="100%"
                          onChange={setText}
                        />
                      </StackItem>
                      {checkError !== null && (
                        <Banner status="warning" title="The schema check could not run" description={checkError} collapsible={false} />
                      )}
                      <StackItem size="static">
                      <VStack
                        gap={1}
                        isScrollable
                        role="status"
                        aria-label="Problems"
                        height={problems.length === 0 || !isChecked ? undefined : appConfig.catalog_problems_max_height_px}
                      >
                        {!isChecked || problems.length === 0 ? (
                          <Text type="supporting" color="secondary">
                            {!unsaved && draft.mode === 'existing' ? 'No changes.' : isChecked ? 'No problems found.' : 'Checking…'}
                          </Text>
                        ) : (
                          problems.map(problem => (
                            <Text key={`${problem.from}:${problem.message}`} type="supporting" color="primary">
                              {problem.message}
                            </Text>
                          ))
                        )}
                      </VStack>
                      </StackItem>
                    </>
                  )}
                </VStack>
                </StackItem>
                {/* The whole border resizes: four overlaid edges on the surface's own
                    edges (it is the nearest positioned ancestor — `position: fixed` —
                    so these are the edges the user drags, not rules inside it), and
                    four corners that move both regions at once. The start-side edges
                    are `isReversed` because a drag outwards grows the box there too.
                    Only the two edges the user is told about carry a pill at rest;
                    eight of them would read as decoration. */}
                <ResizeHandle
                  direction="horizontal"
                  position="overlay"
                  pillPlacement="center"
                  isAlwaysVisible
                  label="Resize catalog dialog"
                  resizable={widthDragProps}
                />
                <ResizeHandle
                  direction="horizontal"
                  position="overlay"
                  pillPlacement="center"
                  isAlwaysVisible={false}
                  isReversed
                  xstyle={frameStyles.startEdge}
                  label="Resize catalog dialog from the left edge"
                  resizable={widthDragProps}
                />
                <ResizeHandle
                  direction="vertical"
                  position="overlay"
                  pillPlacement="center"
                  isAlwaysVisible
                  label="Resize catalog dialog height"
                  resizable={heightDragProps}
                />
                <ResizeHandle
                  direction="vertical"
                  position="overlay"
                  pillPlacement="center"
                  isAlwaysVisible={false}
                  isReversed
                  xstyle={frameStyles.topEdge}
                  label="Resize catalog dialog from the top edge"
                  resizable={heightDragProps}
                />
                <CatalogCornerGrips width={widthRegion.props} height={heightRegion.props} />
              </HStack>
            </LayoutContent>
          }
          footer={
            <LayoutFooter hasDivider>
              <HStack gap={2} justify="between" align="center" width="100%">
                <HStack gap={2}>
                  {draft.mode === 'existing' && (
                    <>
                      <Button label="Delete" variant="destructive" isDisabled={isBusy} onClick={() => setPending({kind: 'delete'})} />
                      <Button label="Reload" variant="ghost" isDisabled={isBusy} onClick={reloadCurrent} />
                      {appConfig.duplicate_enabled ? (
                        <Button label="Duplicate" variant="ghost" isDisabled={isBusy} onClick={startDuplicate} tooltip={`A new ${view.noun} with this text; name it, then Create`} />
                      ) : null}
                    </>
                  )}
                </HStack>
                <HStack gap={2}>
                  <Button label="Close" variant="secondary" isDisabled={isBusy} onClick={requestClose} />
                  {hasEditor && (
                    <Button label={draft.mode === 'new' ? 'Create' : 'Save'} variant="primary" isDisabled={!canSave} isLoading={isBusy} onClick={() => void save()} />
                  )}
                </HStack>
              </HStack>
            </LayoutFooter>
          }
        />
      </Dialog>
      {/* One confirm stands behind every question. It stays mounted and is opened
          by `isOpen` so its own close path runs — that is what returns focus to the
          button that asked — and it starts every answer from unticked. */}
      <CatalogConfirmDialog
        isOpen={pending !== null}
        prompt={promptFor}
        stateNote={isDelete ? view.stateNote : null}
        onCancel={() => setPending(null)}
        onConfirm={confirmPending}
      />
    </>
  );
}
