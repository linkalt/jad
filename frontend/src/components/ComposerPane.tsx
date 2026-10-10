import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import type {ComponentProps, KeyboardEvent, ReactNode, SVGProps} from 'react';
import {
  Badge,
  ChatComposer,
  ChatComposerInput,
  ChatSendButton,
  HStack,
  Icon,
  IconButton,
  useChatPasteAsToken,
} from '@astryxdesign/core';
import type {ChatComposerInputHandle} from '@astryxdesign/core/Chat';
import appConfig from '../config.json';
import {parseAcceptList} from '../image-attachments';
import type {AttachedImage} from '../image-attachments';
import {
  ImageAttachmentPicker,
  ImageAttachmentsDrawer,
  useImageAttachments,
} from './ImageAttachments';
import {fetchSessionFiles} from '../api';
import {createComposerTriggers, resolveMentionFiles} from '../composer-triggers';
import {loadDrafts, saveDrafts} from '../draftStore';
import {carriesReactInternals, noteComposerOperation} from '../desync-watch';
import {
  composerBadgeInsertionPlan,
  composerBadgeSharesLine,
  composerBadgeStartsDraft,
  composerCaretAfterGlue,
  composerPlaceholderForDraft,
  composerTail,
  composerTextInsertionPlan,
  promptNeedsBadge,
} from '../library-insertion';
import type {
  ComposerInsertionPlan,
  ComposerLineBefore,
  ComposerTail,
  ComposerTailNode,
} from '../library-insertion';
import {ComposerPreview} from './ComposerPreview';
import {
  applyInlineMarker,
  applyLinkMarker,
  findEditable,
  INLINE_MARKERS,
  LINK_KEY,
} from './composerMarkdown';

export type ComposerInsertActions = {
  text: (value: string) => void;
  prompt: (value: string, name: string) => void;
  skill: (value: string, name: string) => void;
};

type ComposerPaneProps = {
  /** The session reference used for session-scoped features such as @ file search. */
  activeSessionId: string;
  /** The row identity used to keep each profile view's draft independent. */
  draftKey: string;
  /**
   * Returns true when the message was accepted, so the draft is only cleared on a real send.
   * `images` are the draft's attached images; the text may be empty when there are some.
   */
  onSubmit: (value: string, images: AttachedImage[]) => boolean;
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
  onRegisterInsert?: (insert: ComposerInsertActions | null) => void;
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

/** Put the caret at the end before using the input handle's cursor-based insert APIs. */
function placeCaretAtEnd(root: HTMLElement | null): void {
  const editable = findEditable(root);
  const selection = window.getSelection();
  if (!editable || !selection) return;

  const range = document.createRange();
  range.selectNodeContents(editable);
  range.collapse(false);
  selection.removeAllRanges();
  selection.addRange(range);
}

/** Select the whole draft, so an insert on an empty draft replaces the placeholder break. */
function selectComposerContents(editable: HTMLElement): void {
  const selection = window.getSelection();
  if (!selection) return;

  const range = document.createRange();
  range.selectNodeContents(editable);
  selection.removeAllRanges();
  selection.addRange(range);
}

/**
 * Drop the nodes Chromium keeps for the caret's empty last line.
 *
 * An insert must take the caret's place rather than stack another blank line on
 * top of the placeholder, so the draft is stripped of them before the payload is
 * measured and placed.
 */
function clearTrailingPlaceholder(editable: HTMLElement | null): void {
  if (editable === null) return;
  noteComposerOperation('clearTrailingPlaceholder');
  for (;;) {
    const last = editable.lastChild;
    if (!(last instanceof Text)) return;
    const text = last.textContent ?? '';
    if (text !== '' && text !== '\n') return;
    // Only the composer's own text nodes go: the editable's children are its
    // textual content and Astryx's token spans, and the loop never qualifies an
    // element. Nothing React rendered lives at this level.
    last.remove();
  }
}

/**
 * End the insert's line so the caret waits on the next one.
 *
 * A trailing newline on its own does not hold a caret: the next keystroke is
 * appended to the nearest preceding text node, which puts the user's text back
 * on the payload's line. Chromium's own Shift+Enter leaves a newline *plus* an
 * empty final line and puts the caret in that line, so the insert leaves the
 * same shape — the following keystroke then starts a new line.
 */
function endLineAtCursor(editable: HTMLElement | null): void {
  const selection = window.getSelection();
  if (editable === null || !selection) return;

  noteComposerOperation('endLineAtCursor');
  const home = document.createTextNode('\n');
  editable.append('\n', home);
  const range = document.createRange();
  range.setStart(home, 0);
  range.collapse(true);
  selection.removeAllRanges();
  selection.addRange(range);
}

/** Reduce one composer child node to the fact an insert cares about. */
function describeTailNode(node: ChildNode | null): ComposerTailNode | null {
  if (node === null) return null;
  if (node.nodeType === Node.TEXT_NODE) return {kind: 'text', text: node.textContent ?? ''};
  if (!(node instanceof HTMLElement)) return null;
  if (node.hasAttribute('data-astryx-token')) return {kind: 'token', text: ''};
  if (node.tagName === 'BR') return {kind: 'br', text: ''};
  return {kind: 'text', text: node.textContent ?? ''};
}

function isBadge(node: Node | null): node is HTMLElement {
  return node instanceof HTMLElement && node.hasAttribute('data-astryx-token');
}

/** Which side of a badge the caret sits on. */
type BadgeSide = 'before' | 'after';

/** The badge the caret is parked at, and the side it is parked on. */
interface BadgeAtCaret {
  badge: HTMLElement;
  side: BadgeSide;
}

/**
 * The badge the caret is parked at.
 *
 * A badge cannot hold a caret, so every position the browser leaves beside one is
 * spacer: its glue, or the line its own break opened. Both sides count — the
 * caret at a badge's last character and the caret at its first character are the
 * same place as far as the user is concerned.
 */
function badgeAtCaret(editable: HTMLElement): BadgeAtCaret | null {
  const selection = window.getSelection();
  if (!selection || selection.rangeCount === 0 || !selection.isCollapsed) return null;
  const node = selection.anchorNode;
  const offset = selection.anchorOffset;
  if (node === null || !editable.contains(node)) return null;
  if (isBadge(node)) {
    // A caret anchored inside the token draws at its first character, because a
    // token cannot hold a caret of its own.
    return badgeBeforeCaret(node, offset === 0 ? 'before' : 'after');
  }

  if (node === editable) {
    // A caret on an element boundary: the badge right after it (the caret at the
    // badge's first character) or the one before it, across its glue.
    return badgeAfterCaret(editable.childNodes[offset] ?? null)
      ?? badgeBeforeCaret(editable.childNodes[offset - 1] ?? null, 'after');
  }
  if (node.nodeType !== Node.TEXT_NODE) return null;

  const text = node as Text;
  // Only spacer may sit between the badge and the caret in the caret's own node.
  if (composerCaretAfterGlue(text.data.slice(0, offset))) {
    const behind = badgeBeforeCaret(text.previousSibling, 'after');
    if (behind !== null) return behind;
  }
  // The caret at the badge's first character: right before a badge that starts
  // where the caret's own node ends.
  if (offset !== text.data.length) return null;
  return badgeAfterCaret(text.nextSibling);
}

/**
 * The badge a node follows, across its glue and the line its own break opened.
 *
 * Both are positions the browser leaves beside a token, so a caret there still
 * belongs to that badge — while any text in between means the caret moved on.
 */
function badgeBefore(node: Node | null): HTMLElement | null {
  let crossedBreak = false;
  for (let current = node; current !== null; current = current.previousSibling) {
    if (isBadge(current)) return current;
    if (current.nodeType !== Node.TEXT_NODE) return null;
    const content = current.textContent ?? '';
    if (content === '' || content === '\u00A0') continue;
    if (content === '\n' && !crossedBreak) {
      crossedBreak = true;
      continue;
    }
    return null;
  }
  return null;
}

/** The badge a node starts with, across the spacer in front of it. */
function badgeAhead(node: Node | null): HTMLElement | null {
  for (let current = node; current !== null; current = current.nextSibling) {
    if (isBadge(current)) return current;
    if (current.nodeType !== Node.TEXT_NODE) return null;
    const content = current.textContent ?? '';
    if (content === '' || content === '\u00A0') continue;
    return null;
  }
  return null;
}

/** The badge behind the caret — only while it is the last thing in the draft. */
function badgeBeforeCaret(node: Node | null, side: BadgeSide): BadgeAtCaret | null {
  const badge = badgeBefore(node);
  return badge !== null && !hasContentAfterBadge(badge) ? {badge, side} : null;
}

/** The badge ahead of the caret — only while it is the last thing in the draft. */
function badgeAfterCaret(node: Node | null): BadgeAtCaret | null {
  const badge = badgeAhead(node);
  return badge !== null && !hasContentAfterBadge(badge) ? {badge, side: 'before'} : null;
}

/** Whether a node holds something the user can see. */
function holdsContent(node: Node): boolean {
  return node.nodeType !== Node.TEXT_NODE || !composerCaretAfterGlue(node.textContent ?? '');
}

/** Whether anything the user can see follows the badge in the draft. */
function hasContentAfterBadge(badge: HTMLElement): boolean {
  const after: Node[] = [];
  for (let next = badge.nextSibling; next !== null; next = next.nextSibling) after.push(next);
  return after.some(holdsContent);
}

/** What sits between a badge and the start of its line. */
function lineBeforeBadge(badge: HTMLElement): ComposerLineBefore {
  let text = '';
  let hasInlineBefore = false;
  for (let node: Node | null = badge.previousSibling; node !== null; node = node.previousSibling) {
    if (node.nodeType === Node.TEXT_NODE) {
      const content = node.textContent ?? '';
      const lineBreak = content.lastIndexOf('\n');
      if (lineBreak >= 0) return {text: content.slice(lineBreak + 1) + text, hasInlineBefore, hasBreak: true};
      text = content + text;
      continue;
    }
    if (node.nodeName === 'BR') return {text, hasInlineBefore, hasBreak: true};
    // Another badge, or an element the composer put inline: same line, no break.
    hasInlineBefore = true;
  }
  return {text, hasInlineBefore, hasBreak: false};
}

/** Put a collapsed caret at one offset of a node. */
function setCaretAt(node: Node, offset: number): void {
  const selection = window.getSelection();
  if (selection === null) return;
  const range = document.createRange();
  range.setStart(node, offset);
  range.collapse(true);
  selection.removeAllRanges();
  selection.addRange(range);
}

/**
 * Push the badge at the caret one line down, into the shape a badge insert
 * leaves: the badge ends its line and the caret keeps the side of it the user had.
 *
 * The break goes in front of the badge, because behind it is the one place the
 * browser allows the caret and a break there only pushes the badge's glue around.
 *
 * Returns false when the caret is nowhere near a badge, or when the badge already
 * owns a line of its own below something: then a plain Shift+Enter still breaks
 * the line where the caret is, instead of dragging the badge along.
 */
function moveBadgeToOwnLine(editable: HTMLElement | null): boolean {
  if (editable === null) return false;
  const caret = badgeAtCaret(editable);
  if (caret === null) return false;
  // The token span is Astryx's own node — verified, it carries no React markers.
  // If a version ever rendered it through React, hand-moving it would be the
  // invariant break, so refuse instead of moving what React owns.
  if (carriesReactInternals(caret.badge)) return false;
  const before = lineBeforeBadge(caret.badge);
  if (!composerBadgeSharesLine(before) && !composerBadgeStartsDraft(before)) return false;

  noteComposerOperation('moveBadgeToOwnLine');
  const lineBreak = document.createTextNode('\n');
  caret.badge.parentNode?.insertBefore(lineBreak, caret.badge);
  if (caret.side === 'before') {
    // The caret was at the badge's first character, so it keeps the line above.
    setCaretAt(lineBreak, 0);
    return true;
  }
  clearTrailingPlaceholder(editable);
  endLineAtCursor(editable);
  return true;
}

/**
 * Where an insert lands, read from the composer's nodes rather than from its
 * serialized draft. A badge serializes as the whole prompt and library files end
 * with a newline, so the serialized text of a draft whose badge is still on an
 * open line ends with "\n" — and reading that as a line break is what leaves the
 * next badge sharing the line.
 */
function readComposerTail(editable: HTMLElement | null): ComposerTail {
  if (editable === null) return 'empty';
  const children = Array.from(editable.childNodes);
  const hasContent = children.some(child =>
    child.nodeType === Node.TEXT_NODE
      ? (child.textContent ?? '').trim().length > 0
      : child instanceof HTMLElement && child.tagName !== 'BR',
  );
  return composerTail(describeTailNode(children[children.length - 1] ?? null), hasContent);
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

  const imageLimits = useMemo(
    () => ({
      maxCount: appConfig.attach_image_max_count,
      maxBytes: appConfig.attach_image_max_bytes,
      accept: parseAcceptList(appConfig.attach_image_accept),
    }),
    [],
  );
  const attachments = useImageAttachments(draftKey, imageLimits);
  const openImagePickerRef = useRef<(() => void) | null>(null);
  const hasImages = attachments.images.length > 0;
  const isImageAttachEnabled = appConfig.attach_image_enabled;

  const inputRef = useRef<ChatComposerInputHandle | null>(null);
  // The forwarded ref lands on the composer input's root; the editable is its child.
  const inputRootRef = useRef<HTMLDivElement | null>(null);
  // Set by an outside insert, consumed once the new draft has been committed.
  const focusAfterInsertRef = useRef(false);
  const pasteAsToken = useChatPasteAsToken({
    inputRef,
    threshold: appConfig.paste_as_token_threshold,
  });

  const setDraft = useCallback((sessionId: string, next: string) => {
    setDrafts(current =>
      (current[sessionId] ?? '') === next ? current : {...current, [sessionId]: next},
    );
  }, []);

  /**
   * Shift+Enter next to a badge moves that badge onto its own line.
   *
   * The browser cannot place a caret before a token, so a break typed beside one
   * can only ever land behind it and the badge itself stays put; the composer
   * puts the break in front instead. A badge that already owns its line is left
   * alone, so Shift+Enter still just breaks the line.
   */
  const handleShiftEnter = useCallback(
    (event: KeyboardEvent<HTMLDivElement>): boolean => {
      if (!appConfig.paste_as_token_enabled) return false;
      if (event.key !== 'Enter' || !event.shiftKey) return false;
      if (event.altKey || event.ctrlKey || event.metaKey) return false;
      if (!moveBadgeToOwnLine(findEditable(inputRootRef.current))) return false;
      event.preventDefault();
      const input = inputRef.current;
      if (input !== null) setDraft(draftKey, input.getValue());
      return true;
    },
    [draftKey, setDraft],
  );

  const handleSubmit = useCallback(
    (text: string) => {
      const sent = attachments.images;
      if (onSubmit(text, sent)) {
        setDraft(draftKey, '');
        attachments.clear(sent);
      }
    },
    [attachments, draftKey, onSubmit, setDraft],
  );

  const handleKeyDown = useCallback((event: KeyboardEvent<HTMLDivElement>) => {
    if (handleShiftEnter(event)) {
      return;
    }
    // The composer ignores an empty draft, but a message of images alone is valid.
    if (
      event.key === 'Enter' &&
      !event.shiftKey &&
      !event.altKey &&
      !event.ctrlKey &&
      !event.metaKey &&
      !event.nativeEvent.isComposing &&
      hasImages &&
      !isRunning &&
      (inputRef.current?.getValue() ?? '').trim() === ''
    ) {
      event.preventDefault();
      handleSubmit('');
      return;
    }
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
  }, [handleShiftEnter, handleSubmit, hasImages, isRunning]);

  const handleChange = useCallback(
    (next: string) => {
      setDraft(draftKey, next);
    },
    [draftKey, setDraft],
  );

  /**
   * Append through the Astryx input handle so existing inline badges remain
   * rendered. The serialized value is still the full text sent with the user
   * message, which preserves the existing transcript contract.
   *
   * The plan decides the text around the payload, from the composer's own
   * nodes: a badge takes a line of its own with the caret on the line below it,
   * and an insert into an otherwise empty draft replaces the placeholder break
   * the browser leaves behind instead of landing under an empty first line.
   */
  const insertAtEnd = useCallback(
    (
      insert: (input: ChatComposerInputHandle) => void,
      planFor: (existing: string, tail: ComposerTail) => ComposerInsertionPlan,
    ) => {
      const input = inputRef.current;
      if (input === null) return;
      window.getSelection()?.removeAllRanges();
      input.focus();
      const editable = findEditable(inputRootRef.current);
      // The caret's placeholder line is the insert's position: measure and place
      // the payload from the visible draft, not from the placeholder.
      clearTrailingPlaceholder(editable);
      const plan = planFor(input.getValue(), readComposerTail(editable));
      if (plan.replacesDraft && plan.before.length === 0 && editable !== null) {
        selectComposerContents(editable);
      } else {
        // The handle inserts at the selection; force it to the end so a stale
        // caret from before opening the library cannot replace the first line.
        placeCaretAtEnd(inputRootRef.current);
      }
      if (plan.before.length > 0) input.insertText(plan.before);
      insert(input);
      if (plan.endsLine) endLineAtCursor(editable);
      setDraft(draftKey, input.getValue());
      focusAfterInsertRef.current = true;
    },
    [draftKey, setDraft],
  );

  /** Badges only need the tail; the serialized draft is what lies to them. */
  const badgePlan = useCallback(
    (_existing: string, tail: ComposerTail) => composerBadgeInsertionPlan(tail),
    [],
  );

  const insertText = useCallback(
    (text: string) => insertAtEnd(input => input.insertText(text), composerTextInsertionPlan),
    [insertAtEnd],
  );

  const insertPrompt = useCallback(
    (text: string, name: string) => {
      if (promptNeedsBadge(text.length, appConfig.paste_as_token_enabled, appConfig.paste_as_token_threshold)) {
        noteComposerOperation('insertPromptBadge');
        insertAtEnd(input => input.insertToken({value: text, label: name}), badgePlan);
      } else {
        insertText(text);
      }
    },
    [badgePlan, insertAtEnd, insertText],
  );

  const insertSkill = useCallback(
    (text: string, name: string) => {
      noteComposerOperation('insertSkillBadge');
      return insertAtEnd(
        input =>
          input.insertToken({value: text, render: () => <Badge label={name} />}),
        badgePlan,
      );
    },
    [badgePlan, insertAtEnd],
  );

  const insertActions = useMemo(
    () => ({text: insertText, prompt: insertPrompt, skill: insertSkill}),
    [insertPrompt, insertSkill, insertText],
  );

  useEffect(() => {
    onRegisterInsert?.(insertActions);
    return () => onRegisterInsert?.(null);
  }, [insertActions, onRegisterInsert]);

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
      sendButton={
        <ChatSendButton
          isDisabled={value.trim() === '' && !hasImages}
          onSend={() => handleSubmit(value)}
        />
      }
      onStop={onStop}
      isStopShown={isRunning}
      placeholder={placeholder}
      input={
        <ChatComposerInput
          ref={inputRootRef}
          handleRef={inputRef}
          placeholder={composerPlaceholderForDraft(value, placeholder)}
          triggers={composerTriggers.length > 0 ? composerTriggers : undefined}
          onKeyDown={handleKeyDown}
          onFiles={isImageAttachEnabled ? files => void attachments.add(files) : undefined}
          pasteAsToken={appConfig.paste_as_token_enabled ? pasteAsToken : false}
        />
      }
      drawer={
        <>
          {drawer}
          {isImageAttachEnabled ? (
            <ImageAttachmentsDrawer
              images={attachments.images}
              notice={attachments.notice}
              onRemove={attachments.remove}
            />
          ) : null}
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
          {isImageAttachEnabled ? (
            <>
              <IconButton
                label="Attach image"
                icon={<Icon icon="upload" size="sm" />}
                variant="ghost"
                size="sm"
                onClick={() => openImagePickerRef.current?.()}
              />
              <ImageAttachmentPicker
                accept={appConfig.attach_image_accept}
                onFiles={files => void attachments.add(files)}
                openRef={openImagePickerRef}
              />
            </>
          ) : null}
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
