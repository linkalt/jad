/**
 * @file CatalogEditorDialog.tsx
 * @input Whether the dialog is open, and a callback to close it
 * @output A modal that lists the profile or model files and edits one as raw JSON
 * @position The management surface for <.jad>/profiles and <.jad>/models; every write goes through the catalog routes
 *
 * The text is checked against the kind's scheme as you type; the server's loader
 * has the last word, and its message is shown verbatim when it refuses a write.
 * A save carries the version the file had when it was opened, so an edit made
 * elsewhere in the meantime is refused instead of overwritten.
 */
import {useCallback, useEffect, useMemo, useRef, useState} from 'react';

import {Banner, Button, Dialog, DialogHeader, HStack, Layout, LayoutContent, LayoutFooter, Text, TextInput, VStack} from '@astryxdesign/core';
import {AlertDialog} from '@astryxdesign/core/AlertDialog';
import {EmptyState} from '@astryxdesign/core/EmptyState';
import {List, ListItem} from '@astryxdesign/core/List';
import {SegmentedControl, SegmentedControlItem} from '@astryxdesign/core/SegmentedControl';

import {
  deleteCatalogFile,
  fetchCatalogFile,
  fetchCatalogFiles,
  saveCatalogFile,
  type CatalogFileInfo,
  type CatalogKind,
} from '../api';
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
import {JsonCodeEditor} from './JsonCodeEditor';

const KINDS: readonly CatalogKind[] = ['profiles', 'models'];

type Draft =
  | {mode: 'none'}
  | {mode: 'loading'; id: string}
  | {mode: 'existing'; id: string; etag: string; savedText: string}
  | {mode: 'new'; id: string};

type Pending =
  | {kind: 'switch'; target: string | 'new'}
  | {kind: 'change-kind'; target: CatalogKind}
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
  const [files, setFiles] = useState<CatalogFileInfo[]>([]);
  const [listError, setListError] = useState<string | null>(null);
  const [draft, setDraft] = useState<Draft>({mode: 'none'});
  const [text, setText] = useState('');
  const [newId, setNewId] = useState('');
  const [checked, setChecked] = useState<{text: string; problems: DocumentProblem[]}>({text: '', problems: []});
  const [notice, setNotice] = useState<Notice | null>(null);
  const [isListLoading, setIsListLoading] = useState(true);
  const [checkError, setCheckError] = useState<string | null>(null);
  const [isBusy, setIsBusy] = useState(false);
  const [pending, setPending] = useState<Pending>(null);
  const validatorRef = useRef<ReturnType<typeof createCatalogValidator> | null>(null);
  const loadToken = useRef(0);

  const view = CATALOG_KIND_VIEWS[kind];
  const ids = useMemo(() => sortedIds(files.map(file => file.id)), [files]);
  const savedText = draft.mode === 'existing' ? draft.savedText : draft.mode === 'new' ? view.template : '';
  const unsaved = hasUnsavedWork(draft.mode, text, savedText, newId, view.template);
  const idProblem = draft.mode === 'new' ? newIdProblem(newId, ids, view.noun) : null;
  const isChecked = checked.text === text;
  // The editor keeps the last result while a new check is pending, so squiggles do not blink while typing.
  const problems = checked.problems;

  const reloadList = useCallback(async (signal?: AbortSignal): Promise<CatalogFileInfo[]> => {
    try {
      const next = await fetchCatalogFiles(kind, signal);
      setFiles(next);
      setListError(null);
      setIsListLoading(false);
      return next;
    } catch (caught) {
      if (signal?.aborted) return [];
      setListError(caught instanceof Error ? caught.message : String(caught));
      setIsListLoading(false);
      return [];
    }
  }, [kind]);

  useEffect(() => {
    if (!isOpen) return undefined;
    const controller = new AbortController();
    setIsListLoading(true);
    void reloadList(controller.signal);
    return () => controller.abort();
  }, [isOpen, reloadList]);

  useEffect(() => {
    if (!isOpen) {
      setKind('profiles');
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
    setNotice(null);
    setDraft({mode: 'new', id: ''});
  }, [view.template]);

  const changeKind = (target: CatalogKind): void => {
    loadToken.current += 1;
    setKind(target);
    setFiles([]);
    setListError(null);
    setIsListLoading(true);
    setDraft({mode: 'none'});
    setText('');
    setNewId('');
    setNotice(null);
    setCheckError(null);
    setChecked({text: '', problems: NO_PROBLEMS});
  };

  const requestKind = (target: CatalogKind): void => {
    if (target === kind) return;
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
      await deleteCatalogFile(kind, draft.id, draft.etag);
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
  const title = draft.mode === 'existing' ? draft.id : draft.mode === 'new' ? `New ${view.noun}` : view.plural;

  return (
    <>
      <Dialog
        isOpen={isOpen}
        onOpenChange={open => {
          if (!open) requestClose();
        }}
        width={appConfig.catalog_dialog_width_px}
        purpose="form"
      >
        <Layout
          header={<DialogHeader title="Edit catalog" subtitle={title} onOpenChange={open => { if (!open) requestClose(); }} />}
          content={
            <LayoutContent>
              <HStack gap={4} align="start">
                <VStack gap={2} width={appConfig.catalog_file_list_width_px}>
                  <SegmentedControl label="Catalog" layout="fill" value={kind} onChange={value => requestKind(value as CatalogKind)} isDisabled={isBusy}>
                    {KINDS.map(option => (
                      <SegmentedControlItem key={option} value={option} label={CATALOG_KIND_VIEWS[option].plural} />
                    ))}
                  </SegmentedControl>
                  <Button label={`New ${view.noun}`} variant="primary" width="100%" onClick={() => requestSwitch('new')} isDisabled={isBusy} />
                  {listError !== null ? (
                    <Banner
                      status="error"
                      title={`Could not list ${view.plural.toLowerCase()}`}
                      description={listError}
                      collapsible={false}
                      endContent={<Button label="Retry" size="sm" onClick={() => { setIsListLoading(true); void reloadList(); }} />}
                    />
                  ) : isListLoading ? (
                    <Text type="supporting" color="secondary">Loading {view.plural.toLowerCase()}…</Text>
                  ) : ids.length === 0 ? (
                    <EmptyState title={`No ${view.plural.toLowerCase()}`} description="Create the first one." />
                  ) : (
                    <List density="compact" header={<Text type="label">{view.plural}</Text>}>
                      {ids.map(id => (
                        <ListItem
                          key={id}
                          label={id}
                          isSelected={(draft.mode === 'existing' || draft.mode === 'loading') && draft.id === id}
                          isDisabled={isBusy}
                          onClick={() => requestSwitch(id)}
                        />
                      ))}
                    </List>
                  )}
                </VStack>
                <VStack gap={3} width="100%">
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
                    <EmptyState title={`Select a ${view.noun}`} description="Pick one from the list, or create a new one." />
                  )}
                  {draft.mode === 'loading' && <Text type="supporting" color="secondary">Loading {draft.id}…</Text>}
                  {draft.mode === 'new' && (
                    <TextInput
                      label="Name"
                      description="Written <category>/<name>, for example coding/review."
                      value={newId}
                      onChange={setNewId}
                      hasAutoFocus
                      status={newId !== '' && idProblem !== null ? {type: 'error', message: idProblem} : undefined}
                    />
                  )}
                  {hasEditor && (
                    <>
                      <JsonCodeEditor
                        label={draft.mode === 'new' ? `New ${view.noun} JSON` : `${draft.id} JSON`}
                        value={text}
                        problems={problems}
                        isReadOnly={isBusy}
                        heightPx={appConfig.catalog_editor_height_px}
                        onChange={setText}
                      />
                      {checkError !== null && (
                        <Banner status="warning" title="The schema check could not run" description={checkError} collapsible={false} />
                      )}
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
                    </>
                  )}
                </VStack>
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
                  <Button label={draft.mode === 'new' ? 'Create' : 'Save'} variant="primary" isDisabled={!canSave} isLoading={isBusy} onClick={() => void save()} />
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
            ? `${draft.mode === 'existing' ? draft.id : `This ${view.noun}`} will be removed. The server refuses this while ${view.usedBy} needs it.`
            : 'The text has changes that are not saved.'
        }
        actionLabel={pending?.kind === 'delete' ? 'Delete' : 'Discard'}
        onAction={confirmPending}
      />
    </>
  );
}
