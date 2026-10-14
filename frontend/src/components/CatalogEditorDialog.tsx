/**
 * @file CatalogEditorDialog.tsx
 * @input Whether the dialog is open, and a callback to close it
 * @output A modal that lists the profile, model, session, toolset or MCP server files and edits one as raw JSON
 * @position The management surface for <.jad>/{profiles,models,tools,mcp} and the session files; every write goes through the catalog routes
 *
 * The text is checked against the kind's scheme as you type; the server's loader
 * has the last word, and its message is shown verbatim when it refuses a write.
 * A save carries the version the file had when it was opened, so an edit made
 * elsewhere in the meantime is refused instead of overwritten.
 */
import {useCallback, useEffect, useMemo, useRef, useState} from 'react';

import {Banner, Button, Dialog, HStack, Icon, IconButton, Layout, LayoutContent, LayoutFooter, StackItem, Text, TextInput, VStack} from '@astryxdesign/core';
import {AlertDialog} from '@astryxdesign/core/AlertDialog';
import {CheckboxInput} from '@astryxdesign/core/CheckboxInput';
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
import {FILTER_ALL} from '../catalog-list-view';
import {cappedHeight} from '../catalog-layout';
import appConfig from '../config.json' with {type: 'json'};
import {createCatalogValidator} from '../catalog-validation';
import type {DocumentProblem} from '../json-document';
import {
  CATALOG_KIND_VIEWS,
  canSave as canSaveGate,
  deleteFailure,
  hasUnsavedWork,
  newIdProblem,
  normalizeLineBreaks,
  saveFailure,
  sortedIds,
} from '../catalog-editor-view';
import {CatalogListPane} from './CatalogListPane';
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
  | null;

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
  const [deleteState, setDeleteState] = useState(false);
  const [pending, setPending] = useState<Pending>(null);
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState(FILTER_ALL);
  const [tab, setTab] = useState<EditorTab>('editor');
  const validatorRef = useRef<ReturnType<typeof createCatalogValidator> | null>(null);
  const loadToken = useRef(0);

  const view = CATALOG_KIND_VIEWS[kind];
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
      setTab('editor');
      setDeleteState(false);
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
    setDeleteState(false);
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
    setDeleteState(false);
    setNotice(null);
    setDraft({mode: 'new', id: ''});
  }, [view.template]);

  const changeKind = (target: CatalogKind): void => {
    loadToken.current += 1;
    setKind(target);
    setQuery('');
    setFilter(FILTER_ALL);
    setTab('editor');
    setDeleteState(false);
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
    setDeleteState(false);
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

  const confirmPending = (): void => {
    const action = pending;
    setPending(null);
    if (!action) return;
    if (action.kind === 'close') onClose();
    else if (action.kind === 'change-kind') changeKind(action.target);
    else if (action.kind === 'back') backToList();
    else if (action.kind === 'switch') {
      if (action.target === 'new') startNew();
      else void openExisting(action.target);
    } else if (action.kind === 'delete') void performDelete();
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

  const performDelete = async (): Promise<void> => {
    if (draft.mode !== 'existing') return;
    setIsBusy(true);
    setNotice(null);
    try {
      await deleteCatalogFile(kind, draft.id, draft.etag, {deleteState});
      const removed = draft.id;
      setDraft({mode: 'none'});
      setText('');
      setDeleteState(false);
      setNotice({kind: 'success', title: 'Done', message: `Deleted ${removed}.`});
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

  return (
    <>
      <Dialog
        isOpen={isOpen}
        onOpenChange={open => {
          if (!open) requestClose();
        }}
        width={appConfig.catalog_dialog_width_px}
        maxHeight={appConfig.catalog_dialog_max_height}
        purpose="form"
        aria-label={`Catalog: ${title}`}
      >
        <Layout
          content={
            <LayoutContent padding={0}>
              <HStack gap={3} align="stretch" padding={4} height={cappedHeight(appConfig.catalog_body_height_px, appConfig.catalog_body_viewport_percent)}>
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
                      onQueryChange={setQuery}
                      onFilterChange={setFilter}
                      onOpen={id => requestSwitch(id)}
                      onNew={() => requestSwitch('new')}
                      onRetry={() => void reloadList()}
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
                    <TabList aria-label="Editor view" value={tab} onChange={value => setTab(value as EditorTab)} hasDivider>
                      <Tab value="editor" label="JSON" />
                      <Tab value="schema" label="Schema" />
                    </TabList>
                  )}
                  {hasEditor && tab === 'schema' && (
                    <StackItem size="fill" isScrollable>
                      <CatalogSchemaTab kind={kind} />
                    </StackItem>
                  )}
                  {draft.mode === 'existing' && kind === 'sessions' && tab === 'editor' && (
                    <CheckboxInput
                      label="Also delete its transcripts and tool state when deleting"
                      description="Off keeps the state folder, so a session created again under this name finds it."
                      value={deleteState}
                      onChange={setDeleteState}
                      isDisabled={isBusy}
                    />
                  )}
                  {hasEditor && tab === 'editor' && (
                    <>
                      <StackItem size="fill">
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
      <AlertDialog
        isOpen={pending !== null}
        onOpenChange={open => {
          if (!open) setPending(null);
        }}
        title={pending?.kind === 'delete' ? `Delete this ${view.noun}?` : 'Discard your changes?'}
        description={
          pending?.kind === 'delete'
            ? `${draft.mode === 'existing' ? draft.id : `This ${view.noun}`} will be removed. ${view.deleteNote}${kind === 'sessions' ? (deleteState ? ' Its transcripts and tool state are deleted too.' : ' Its transcripts and tool state are kept.') : ''}`
            : 'The text has changes that are not saved.'
        }
        actionLabel={pending?.kind === 'delete' ? 'Delete' : 'Discard'}
        onAction={confirmPending}
      />
    </>
  );
}
