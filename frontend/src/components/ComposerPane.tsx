import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import type {ComponentProps, KeyboardEvent, ReactNode, SVGProps} from 'react';
import {
  ChatComposer,
  ChatComposerInput,
  HStack,
  Icon,
  IconButton,
  useChatPasteAsToken,
} from '@astryxdesign/core';
import type {ChatComposerInputHandle} from '@astryxdesign/core';
import appConfig from '../config.json';
import {fetchSessionFiles} from '../api';
import {createComposerTriggers, resolveMentionFiles} from '../composer-triggers';
import {loadDrafts, saveDrafts} from '../draftStore';
import {ComposerPreview} from './ComposerPreview';
import {
  applyInlineMarker,
  applyLinkMarker,
  findEditable,
  INLINE_MARKERS,
  LINK_KEY,
} from './composerMarkdown';

type ComposerPaneProps = {
  /** The session reference used for session-scoped features such as @ file search. */
  activeSessionId: string;
  /** The row identity used to keep each profile view's draft independent. */
  draftKey: string;
  /** Returns true when the message was accepted, so the draft is only cleared on a real send. */
  onSubmit: (value: string) => boolean;
  onStop: () => void;
  isRunning: boolean;
  placeholder: string;
  /** Whether the run inspector is open. Its toggle is the only way back once it is closed. */
  showInspector: boolean;
  onToggleInspector: () => void;
  /** Whether the left data panel is open, and which dataset the toggle would open. */
  isLeftPanelOpen: boolean;
  leftPanelLabel: string;
  onToggleLeftPanel: () => void;
  /**
   * Hands the pane's insert function to the app, so a view outside the composer
   * (the prompt library) can load text into the active draft. Drafts have to
   * stay in this component — see the note below — so they cannot be set from
   * outside without a registered writer like this one.
   */
  onRegisterInsert?: (insert: ((text: string) => void) | null) => void;
  drawer?: ReactNode;
  /** Content actions (attach, usage, compact), placed in the bottom action bar. */
  toolbarActions?: ReactNode;
  /** Informational text shown after the actions (shortcuts hint, compaction feedback). */
  footerNote?: ReactNode;
  status?: ComponentProps<typeof ChatComposer>['status'];
};

/**
 * The semantic icon set ships no preview/eye glyph (`eyeSlash` only), so the
 * markdown preview toggle supplies an outline SVG of its own. `Icon` accepts a
 * component directly and applies the size and colour styling.
 */
function PreviewIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5} {...props}>
      <path
        d="M2.5 12S6 5.75 12 5.75 21.5 12 21.5 12 18 18.25 12 18.25 2.5 12 2.5 12Z"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <circle cx="12" cy="12" r="2.75" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/**
 * Owns the composer's draft state.
 *
 * This state deliberately lives below App: when it sat in the root component,
 * every keystroke re-rendered the whole conversation, and typing became
 * noticeably slow once the session history grew (measured ~261ms per
 * keystroke with 187 turns versus ~30ms with none). Keeping drafts here means
 * typing re-renders only the composer.
 *
 * The pane is also the app's single action surface: every control sits in the
 * composer's top row — the left panel toggle, the content actions it is given
 * and the markdown preview toggle on the left, the run inspector toggle on the
 * right. Only the send button is in the bottom row.
 */
export function ComposerPane({
  activeSessionId,
  draftKey,
  onSubmit,
  onStop,
  isRunning,
  placeholder,
  showInspector,
  onToggleInspector,
  isLeftPanelOpen,
  leftPanelLabel,
  onToggleLeftPanel,
  onRegisterInsert,
  drawer,
  toolbarActions,
  footerNote,
  status,
}: ComposerPaneProps) {
  const [drafts, setDrafts] = useState<Record<string, string>>(() => loadDrafts());
  const [isPreviewOpen, setIsPreviewOpen] = useState(false);
  const value = drafts[draftKey] ?? '';
  const isPreviewEnabled = appConfig.composer_preview_enabled;

  /**
   * The trigger menus. Rebuilt only when the session changes: the `@` menu now
   * lists the active session's files, and handing the composer a fresh array on
   * every render would restart its trigger search while the user is typing.
   */
  const composerTriggers = useMemo(
    () =>
      appConfig.mention_files_enabled
        ? createComposerTriggers({
            files: resolveMentionFiles(appConfig.mention_files),
            maxResults: appConfig.mention_max_results,
            sessionFiles:
              appConfig.mention_source === 'cwd'
                ? signal =>
                    fetchSessionFiles(activeSessionId, signal).then(listing => listing.files)
                : undefined,
          })
        : [],
    [activeSessionId],
  );

  const inputRef = useRef<ChatComposerInputHandle | null>(null);
  // The forwarded ref lands on the composer input's root; the editable is its child.
  const inputRootRef = useRef<HTMLDivElement | null>(null);
  // Set by an outside insert, consumed once the new draft has been committed.
  const focusAfterInsertRef = useRef(false);
  const pasteAsToken = useChatPasteAsToken({
    inputRef,
    threshold: appConfig.paste_as_token_threshold,
  });

  const handleKeyDown = useCallback((event: KeyboardEvent<HTMLDivElement>) => {
    if (!appConfig.composer_markdown_shortcuts) {
      return;
    }
    if (!(event.metaKey || event.ctrlKey) || event.altKey || event.shiftKey) {
      return;
    }
    const editable = findEditable(inputRootRef.current);
    if (!editable) {
      return;
    }
    const key = event.key.toLowerCase();
    if (key === LINK_KEY) {
      event.preventDefault();
      applyLinkMarker(editable);
      return;
    }
    const marker = INLINE_MARKERS[key];
    if (marker) {
      event.preventDefault();
      applyInlineMarker(editable, marker);
    }
  }, []);

  const setDraft = useCallback((sessionId: string, next: string) => {
    setDrafts(current =>
      (current[sessionId] ?? '') === next ? current : {...current, [sessionId]: next},
    );
  }, []);

  const handleChange = useCallback(
    (next: string) => {
      setDraft(draftKey, next);
    },
    [draftKey, setDraft],
  );

  const handleSubmit = useCallback(
    (text: string) => {
      if (onSubmit(text)) {
        setDraft(draftKey, '');
      }
    },
    [draftKey, onSubmit, setDraft],
  );

  /**
   * Append text to the active draft. Appending rather than replacing means a
   * half-written message survives picking a prompt; an empty draft just becomes
   * the text.
   */
  const insertIntoDraft = useCallback(
    (text: string) => {
      setDrafts(current => {
        const existing = current[draftKey] ?? '';
        return {
          ...current,
          [draftKey]: existing ? `${existing}\n\n${text}` : text,
        };
      });
      focusAfterInsertRef.current = true;
    },
    [draftKey],
  );

  useEffect(() => {
    onRegisterInsert?.(insertIntoDraft);
    return () => onRegisterInsert?.(null);
  }, [insertIntoDraft, onRegisterInsert]);

  /**
   * Drafts are user content, so they outlive the tab: the newest draft is
   * written on a short delay rather than on every keystroke, and flushed when
   * the page goes away so a reload cannot beat the timer.
   */
  const draftsRef = useRef(drafts);
  useEffect(() => {
    draftsRef.current = drafts;
  }, [drafts]);
  useEffect(() => {
    const handle = window.setTimeout(
      () => saveDrafts(drafts),
      appConfig.composer_draft_save_delay_ms,
    );
    return () => window.clearTimeout(handle);
  }, [drafts]);
  useEffect(() => {
    const flush = () => saveDrafts(draftsRef.current);
    window.addEventListener('pagehide', flush);
    return () => window.removeEventListener('pagehide', flush);
  }, []);

  // Focus after the draft has rendered, so the caret lands at the end of the
  // text that was just inserted rather than the text it replaced.
  useEffect(() => {
    if (focusAfterInsertRef.current) {
      focusAfterInsertRef.current = false;
      inputRef.current?.focus();
    }
  });

  return (
    <ChatComposer
      value={value}
      onChange={handleChange}
      onSubmit={handleSubmit}
      onStop={onStop}
      isStopShown={isRunning}
      placeholder={placeholder}
      input={
        <ChatComposerInput
          ref={inputRootRef}
          handleRef={inputRef}
          triggers={composerTriggers.length > 0 ? composerTriggers : undefined}
          onKeyDown={handleKeyDown}
          pasteAsToken={appConfig.paste_as_token_enabled ? pasteAsToken : false}
        />
      }
      drawer={
        <>
          {drawer}
          {isPreviewEnabled && isPreviewOpen ? <ComposerPreview value={value} /> : null}
        </>
      }
      headerActions={
        <HStack align="center" gap={1}>
          <IconButton
            data-panel-toggle=""
            label={`${isLeftPanelOpen ? 'Hide' : 'Show'} ${leftPanelLabel}`}
            icon={
              <Icon icon={isLeftPanelOpen ? 'chevronLeft' : 'chevronRight'} size="sm" />
            }
            variant={isLeftPanelOpen ? 'secondary' : 'ghost'}
            size="sm"
            onClick={onToggleLeftPanel}
          />
          {toolbarActions}
          {isPreviewEnabled ? (
            <IconButton
              label={isPreviewOpen ? 'Hide markdown preview' : 'Preview markdown'}
              icon={<Icon icon={PreviewIcon} size="sm" />}
              variant={isPreviewOpen ? 'secondary' : 'ghost'}
              size="sm"
              onClick={() => setIsPreviewOpen(current => !current)}
            />
          ) : null}
        </HStack>
      }
      headerContext={
        <IconButton
          data-panel-toggle=""
          label={showInspector ? 'Hide run inspector' : 'Show run inspector'}
          icon={<Icon icon="viewColumns" size="sm" />}
          variant={showInspector ? 'secondary' : 'ghost'}
          size="sm"
          onClick={onToggleInspector}
        />
      }
      footerActions={footerNote}
      status={status}
      elevation="low"
    />
  );
}
