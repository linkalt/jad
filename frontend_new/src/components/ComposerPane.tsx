import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import type {KeyboardEvent, ReactNode, SVGProps} from 'react';
import {
  Badge,
  Button,
  ChatComposer,
  ChatComposerInput,
  ChatSendButton,
  HStack,
  Icon,
  IconButton,
  Token,
  useChatPasteAsToken,
} from '@astryxdesign/core';
import type {ChatComposerInputHandle} from '@astryxdesign/core/Chat';
import appConfig from '../config.json';
import {fileRule, parseExtensionList, parseMimeList} from '../file-intake';
import {parseAcceptList} from '../image-attachments';
import type {AttachedFile, AttachedImage} from '../image-attachments';
import {
  ImageAttachmentPicker,
  ImageAttachmentsDrawer,
  useAttachments,
  useImageAttachments,
} from './ImageAttachments';
import {FileAttachmentPicker, FileIcon} from './FileAttachments';
import {readCaretEdges} from './composerCaret';
import {IDLE_HISTORY, stepHistory, type HistoryCursor} from '../input-history';
import {
  expandRootLibraryCommand,
  fetchRootLibrary,
  fetchRootLibraryAsset,
  fetchSessionFiles,
  fetchSessionMCPPrompt,
  fetchSessionMCPPrompts,
  fetchSessionMCPResource,
  fetchSessionMCPResources,
  type MCPPrompt,
  type MCPResource,
  type CommandRunOptions,
  type RootLibraryCommand,
} from '../api';
import {
  createSlashSource,
  mcpPromptArgumentValues,
  mcpResourceVariableValues,
  type SlashCommand,
  type SlashSelection,
} from '../slash-commands';
import {MCPPromptDialog} from './MCPPromptDialog';
import {CommandArgumentsDialog} from './CommandArgumentsDialog';
import {ResourceVariablesDialog} from './ResourceVariablesDialog';
import {useFileDrop} from './FileDrop';
import {createComposerTriggers, resolveMentionFiles, slashTrigger} from '../composer-triggers';
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

/**
 * How a message is sent: `plan` runs it in plan mode; `model` and `tools` come from a
 * custom command's front matter; `executePlan` carries out the plan the view ends with,
 * and the text may then be empty.
 */
export type ComposerSubmitOptions = {
  plan: boolean;
  model?: string;
  tools?: string[];
  executePlan?: boolean;
};

/** The run options a picked custom command left for the view's next message. */
type PendingCommandOptions = {command: string; model?: string; tools?: string[]};

/** The token text of pending command options: the command, then what it sets. */
export function commandOptionsLabel(pending: PendingCommandOptions): string {
  const parts = [
    ...(pending.model === undefined ? [] : [`model ${pending.model}`]),
    ...(pending.tools === undefined ? [] : [`tools ${pending.tools.join(', ')}`]),
  ];
  return `/${pending.command}: ${parts.join(' · ')}`;
}

export type ComposerInsertActions = {
  text: (value: string) => void;
  prompt: (value: string, name: string) => void;
  skill: (value: string, name: string) => void;
};

type ComposerPaneProps = {
  /** The session reference used for session-scoped features such as @ file search. */
  activeSessionId: string;
  /** The profile the view is pinned to, for its MCP prompts; undefined is the session's default. */
  activeProfile?: string;
  /** The row identity used to keep each profile view's draft independent. */
  draftKey: string;
  /** Earlier user messages of this view, oldest first, for ArrowUp / ArrowDown recall. */
  inputHistory: readonly string[];
  /**
   * Returns true when the message was accepted, so the draft is only cleared on a real send.
   * `images` are the draft's attached images; the text may be empty when there are some.
   */
  onSubmit: (
    value: string,
    images: AttachedImage[],
    files: AttachedFile[],
    options: ComposerSubmitOptions,
  ) => boolean;
  onStop: () => void;
  isRunning: boolean;
  /** Turns off typing, sending, attaching, dropping and inserting. */
  isDisabled?: boolean;
  placeholder: string;
  /** Whether the run inspector is open. Its toggle is the only way back once it is closed. */
  showInspector: boolean;
  onToggleInspector: () => void;
  /** Whether the left data panel is open, and which dataset the toggle would open. */
  isPromptLibraryOpen: boolean;
  onTogglePromptLibrary: () => void;
  /**
   * Hands the pane's insert function to the app, so a view outside the composer
   * (the prompt library) can load text into the active draft. Drafts have to
   * stay in this component — see the note below — so they cannot be set from
   * outside without a registered writer like this one.
   */
  onRegisterInsert?: (insert: ComposerInsertActions | null) => void;
  /** The built-in `/` commands this view can run; none leaves only the library entries. */
  slashCommands?: readonly SlashCommand[];
  /** Runs the picked built-in command. */
  onSlashCommand?: (name: SlashCommand['name']) => void;
  /** Reports a `/` library item that could not be loaded. */
  onSlashError?: (message: string) => void;
  drawer?: ReactNode;
  /** Content actions (model, usage, state context), placed at the start of the header row. */
  toolbarActions?: ReactNode;
  /**
   * Row controls placed with the run inspector at the end of the header row, where
   * the row is right-aligned: the conversation actions and the compaction control.
   */
  headerContextActions?: ReactNode;
  /** Informational text shown after the actions (shortcuts hint, compaction feedback). */
  footerNote?: ReactNode;
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
 * The plan-mode switch: a list with check marks, drawn here because the semantic
 * set has no checklist glyph.
 */
function PlanIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5} {...props}>
      <path d="M10 6.5h10M10 12h10M10 17.5h10" strokeLinecap="round" />
      <path d="M3.5 6.5l1.5 1.5 2.5-3M3.5 12l1.5 1.5 2.5-3M3.5 17.5l1.5 1.5 2.5-3" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/**
 * The semantic set's `upload` glyph is an arrow into a tray, not a picture, so the
 * attach-image button draws its own: a frame with a mountain and a sun (the
 * svgrepo `image` outline). The source SVG's white background plate and its
 * 800px box are dropped, and its black stroke becomes `currentColor`, so the
 * theme governs the colour and `Icon` governs the size.
 */
function ImageIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5} {...props}>
      <path
        d="M21 16V20C21 20.5523 20.5523 21 20 21H4C3.44772 21 3 20.5523 3 20V18M21 16V4C21 3.44772 20.5523 3 20 3H4C3.44772 3 3 3.44772 3 4V18M21 16L15.4829 12.3219C15.1843 12.1228 14.8019 12.099 14.4809 12.2595L3 18"
        strokeLinejoin="round"
      />
      <circle cx="8" cy="9" r="2" strokeLinejoin="round" />
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

/** The composer while a file is dragged over it: a token-coloured border and an inset. */
const DROP_HIGHLIGHT = {
  outline: 'var(--border-width) dashed var(--color-border-blue)',
  outlineOffset: 'calc(-1 * var(--spacing-1))',
  borderRadius: 'var(--radius-container)',
} as const;

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
  activeProfile,
  draftKey,
  inputHistory,
  onSubmit,
  onStop,
  isRunning,
  isDisabled = false,
  placeholder,
  showInspector,
  onToggleInspector,
  isPromptLibraryOpen,
  onTogglePromptLibrary,
  onRegisterInsert,
  slashCommands,
  onSlashCommand,
  onSlashError,
  drawer,
  toolbarActions,
  headerContextActions,
  footerNote,
}: ComposerPaneProps) {
  const [drafts, setDrafts] = useState<Record<string, string>>(() => loadDrafts());
  const [isPreviewOpen, setIsPreviewOpen] = useState(false);
  const value = drafts[draftKey] ?? '';
  const isPreviewEnabled = appConfig.composer_preview_enabled;
  // Plan mode is per view and stays on until it is switched off, so a plan can be
  // refined over several messages before the work is asked for.
  const [planViews, setPlanViews] = useState<Record<string, boolean>>({});
  const isPlanModeEnabled = appConfig.plan_mode_enabled;
  const isPlan = isPlanModeEnabled && planViews[draftKey] === true;
  const togglePlan = useCallback(() => {
    setPlanViews(current => ({...current, [draftKey]: current[draftKey] !== true}));
  }, [draftKey]);
  // A custom command's model and tools ride on the view's next message, shown as a token
  // the person can remove; its plan: true turns plan mode on.
  const [commandOptionViews, setCommandOptionViews] = useState<Record<string, PendingCommandOptions>>({});
  const pendingCommand = commandOptionViews[draftKey];
  const clearPendingCommand = useCallback(() => {
    setCommandOptionViews(current => {
      if (current[draftKey] === undefined) return current;
      const next = {...current};
      delete next[draftKey];
      return next;
    });
  }, [draftKey]);
  const applyCommandOptions = useCallback(
    (command: string, options: CommandRunOptions) => {
      if (options.plan === true && isPlanModeEnabled) {
        setPlanViews(current => ({...current, [draftKey]: true}));
      }
      if (options.model === undefined && options.tools === undefined) return;
      setCommandOptionViews(current => ({
        ...current,
        [draftKey]: {
          command,
          ...(options.model === undefined ? {} : {model: options.model}),
          ...(options.tools === undefined ? {} : {tools: options.tools}),
        },
      }));
    },
    [draftKey, isPlanModeEnabled],
  );

  /**
   * The trigger menus. Rebuilt only when the session changes: the `@` menu now
   * lists the active session's files, and handing the composer a fresh array on
   * every render would restart its trigger search while the user is typing.
   */
  const slashPickRef = useRef<(selection: SlashSelection) => void>(() => undefined);
  const composerTriggers = useMemo(() => {
    const slash = appConfig.slash_menu_enabled
      ? {
          source: createSlashSource({
            commands: slashCommands ?? [],
            loadCatalog: () => fetchRootLibrary(),
            loadMCPPrompts: appConfig.slash_menu_mcp_prompts_enabled
              ? () => fetchSessionMCPPrompts(activeSessionId, activeProfile).then(listed => listed.prompts)
              : undefined,
            loadMCPResources: appConfig.slash_menu_mcp_resources_enabled
              ? () => fetchSessionMCPResources(activeSessionId, activeProfile).then(listed => listed.resources)
              : undefined,
            maxResults: appConfig.slash_menu_max_results,
            refreshMs: appConfig.slash_menu_refresh_ms,
          }),
          onPick: (selection: SlashSelection) => slashPickRef.current(selection),
        }
      : undefined;
    if (!appConfig.mention_files_enabled) {
      return slash ? [slashTrigger(slash.source, slash.onPick)] : [];
    }
    return createComposerTriggers({
      files: resolveMentionFiles(appConfig.mention_files),
      maxResults: appConfig.mention_max_results,
      sessionFiles:
        appConfig.mention_source === 'cwd'
          ? signal => fetchSessionFiles(activeSessionId, signal).then(listing => listing.files)
          : undefined,
      slash,
    });
  }, [activeSessionId, activeProfile, slashCommands]);

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
  const isImageAttachEnabled = appConfig.attach_image_enabled;
  const fileLimits = useMemo(
    () =>
      fileRule({
        maxCount: appConfig.attach_file_max_count,
        maxBytes: appConfig.attach_file_max_bytes,
        mimeTypes: parseMimeList(appConfig.attach_file_accept),
        extensions: parseExtensionList(appConfig.attach_file_accept),
      }),
    [],
  );
  const fileAttachments = useAttachments(draftKey, fileLimits, 'file');
  const openFilePickerRef = useRef<(() => void) | null>(null);
  const isFileAttachEnabled = appConfig.attach_file_enabled;
  // A message of attachments alone is valid, so the send button and Enter treat it as content.
  const hasAttachments = attachments.images.length > 0 || fileAttachments.images.length > 0;

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

  const historyCursorRef = useRef<HistoryCursor>(IDLE_HISTORY);
  useEffect(() => {
    historyCursorRef.current = IDLE_HISTORY;
  }, [draftKey]);

  const handleHistoryKey = useCallback(
    (event: KeyboardEvent<HTMLDivElement>): boolean => {
      if (event.key !== 'ArrowUp' && event.key !== 'ArrowDown') return false;
      if (event.shiftKey || event.altKey || event.ctrlKey || event.metaKey) return false;
      if (event.nativeEvent.isComposing || inputHistory.length === 0) return false;
      const edges = readCaretEdges(findEditable(inputRootRef.current));
      const input = inputRef.current;
      if (edges === null || input === null) return false;
      const step = stepHistory(
        inputHistory,
        historyCursorRef.current,
        input.getValue(),
        event.key,
        edges,
      );
      if (step === null) return false;
      event.preventDefault();
      historyCursorRef.current = step.cursor;
      setDraft(draftKey, step.text);
      return true;
    },
    [draftKey, inputHistory, setDraft],
  );

  const handleSubmit = useCallback(
    (text: string) => {
      if (isDisabled) {
        return;
      }
      const sentImages = attachments.images;
      const sentFiles = fileAttachments.images;
      const options: ComposerSubmitOptions = {
        plan: isPlan,
        ...(pendingCommand?.model === undefined ? {} : {model: pendingCommand.model}),
        ...(pendingCommand?.tools === undefined ? {} : {tools: pendingCommand.tools}),
      };
      if (onSubmit(text, sentImages, sentFiles, options)) {
        historyCursorRef.current = IDLE_HISTORY;
        setDraft(draftKey, '');
        attachments.clear(sentImages);
        fileAttachments.clear(sentFiles);
        clearPendingCommand();
      }
    },
    [attachments, clearPendingCommand, draftKey, fileAttachments, isDisabled, isPlan, onSubmit, pendingCommand, setDraft],
  );

  // Execute plan: the next message leaves plan mode and carries out the plan, with the
  // draft's text, if any, after the server's execute prompt. Attachments stay in the draft.
  const executePlan = useCallback(() => {
    if (isDisabled) return;
    const text = inputRef.current?.getValue() ?? value;
    if (onSubmit(text, [], [], {plan: false, executePlan: true})) {
      historyCursorRef.current = IDLE_HISTORY;
      setDraft(draftKey, '');
      setPlanViews(current => ({...current, [draftKey]: false}));
      clearPendingCommand();
    }
  }, [clearPendingCommand, draftKey, isDisabled, onSubmit, setDraft, value]);

  const handleKeyDown = useCallback((event: KeyboardEvent<HTMLDivElement>) => {
    if (handleShiftEnter(event) || handleHistoryKey(event)) {
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
      hasAttachments &&
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
  }, [handleHistoryKey, handleShiftEnter, handleSubmit, hasAttachments, isRunning]);

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

  /** The MCP prompt whose arguments the dialog is asking for. */
  const [argumentPrompt, setArgumentPrompt] = useState<MCPPrompt | null>(null);
  // An MCP prompt is filled by its server and inserted like a library prompt, by the
  // same length rule; its badge is named <server>:<prompt>.
  const insertMCPPrompt = (prompt: MCPPrompt, values: Record<string, string>) => {
    fetchSessionMCPPrompt(
      activeSessionId,
      {server: prompt.server, name: prompt.name, arguments: mcpPromptArgumentValues(prompt, values)},
      activeProfile,
    ).then(
      text => insertPrompt(text, `${prompt.server}:${prompt.name}`),
      reason => onSlashError?.(reason instanceof Error ? reason.message : String(reason)),
    );
  };

  /** The MCP resource template whose variables the dialog is asking for. */
  const [variableResource, setVariableResource] = useState<MCPResource | null>(null);
  // An MCP resource is read by its server and inserted like a library prompt, as a badge
  // named <server>:<resource> past the length rule.
  const insertResource = (resource: MCPResource, values: Record<string, string>) => {
    fetchSessionMCPResource(activeSessionId, resource, mcpResourceVariableValues(resource, values), activeProfile).then(
      text => insertPrompt(text, `${resource.server}:${resource.name}`),
      reason => onSlashError?.(reason instanceof Error ? reason.message : String(reason)),
    );
  };

  /** The custom command whose arguments the dialog is asking for. */
  const [argumentCommand, setArgumentCommand] = useState<RootLibraryCommand | null>(null);
  // The server fills a custom command, as jad run has it filled; the text is inserted
  // like a library prompt, as a badge named /<command> past the length rule.
  const insertCommand = (command: RootLibraryCommand, args: string) => {
    expandRootLibraryCommand(command.id, args).then(
      expanded => {
        insertPrompt(expanded.text, `/${command.command}`);
        applyCommandOptions(command.command, expanded.options);
      },
      reason => onSlashError?.(reason instanceof Error ? reason.message : String(reason)),
    );
  };

  // A library pick inserts the way the prompt library panel does: a prompt by its
  // length rule, a template as plain text, a skill as a badge.
  slashPickRef.current = selection => {
    if (selection.type === 'resource') {
      if (selection.resource.uriTemplate !== undefined && selection.resource.variables.length > 0) {
        setVariableResource(selection.resource);
      } else {
        insertResource(selection.resource, {});
      }
      return;
    }
    if (selection.type === 'custom') {
      if (selection.command.takesArguments) setArgumentCommand(selection.command);
      else insertCommand(selection.command, '');
      return;
    }
    if (selection.type === 'command') {
      if (selection.name === 'plan') togglePlan();
      else onSlashCommand?.(selection.name);
      return;
    }
    if (selection.type === 'mcp') {
      if (selection.prompt.arguments.length === 0) insertMCPPrompt(selection.prompt, {});
      else setArgumentPrompt(selection.prompt);
      return;
    }
    fetchRootLibraryAsset(selection.kind, selection.id).then(
      asset => {
        if (asset.kind === 'prompt') insertPrompt(asset.content, asset.name);
        else if (asset.kind === 'template') insertText(asset.content);
        else insertSkill(asset.content, asset.name);
      },
      reason => onSlashError?.(reason instanceof Error ? reason.message : String(reason)),
    );
  };

  const dropRules = useMemo(
    () => ({
      imageAccept: parseAcceptList(appConfig.attach_image_accept),
      fileAccept: parseMimeList(appConfig.attach_file_accept),
      fileExtensions: parseExtensionList(appConfig.attach_file_accept),
      isImageEnabled: appConfig.attach_image_enabled,
      isFileEnabled: appConfig.attach_file_enabled,
    }),
    [],
  );
  const fileDrop = useFileDrop({
    rules: dropRules,
    addImages: attachments.add,
    addFiles: fileAttachments.add,
  });
  const isDropEnabled = appConfig.drop_files_enabled && !isDisabled;

  useEffect(() => {
    onRegisterInsert?.(isDisabled ? null : insertActions);
    return () => onRegisterInsert?.(null);
  }, [insertActions, isDisabled, onRegisterInsert]);

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
    <>
      <ResourceVariablesDialog
        resource={variableResource}
        onCancel={() => setVariableResource(null)}
        onSubmit={(resource, values) => {
          setVariableResource(null);
          insertResource(resource, values);
        }}
      />
      <CommandArgumentsDialog
        command={argumentCommand}
        onCancel={() => setArgumentCommand(null)}
        onSubmit={(command, args) => {
          setArgumentCommand(null);
          insertCommand(command, args);
        }}
      />
      <MCPPromptDialog
        prompt={argumentPrompt}
        onCancel={() => setArgumentPrompt(null)}
        onSubmit={(prompt, values) => {
          setArgumentPrompt(null);
          insertMCPPrompt(prompt, values);
        }}
      />
      <ChatComposer
        {...(isDropEnabled ? fileDrop.targetProps : {})}
        style={isDropEnabled && fileDrop.isDragging ? DROP_HIGHLIGHT : undefined}
        isDisabled={isDisabled}
        value={value}
        onChange={handleChange}
        onSubmit={handleSubmit}
        sendButton={
          <ChatSendButton
            isDisabled={isDisabled || (value.trim() === '' && !hasAttachments)}
            onSend={() => handleSubmit(value)}
          />
        }
        onStop={onStop}
        isStopShown={isRunning && !isDisabled}
        placeholder={placeholder}
        input={
          <ChatComposerInput
            ref={inputRootRef}
            handleRef={inputRef}
            placeholder={composerPlaceholderForDraft(
              value,
              isPlan ? appConfig.plan_mode_placeholder : placeholder,
            )}
            triggers={composerTriggers.length > 0 ? composerTriggers : undefined}
            onKeyDown={handleKeyDown}
            hasHistory={false}
            onFiles={
              isImageAttachEnabled || isFileAttachEnabled ? fileDrop.handleFiles : undefined
            }
            pasteAsToken={appConfig.paste_as_token_enabled ? pasteAsToken : false}
          />
        }
        drawer={
          <>
            {isDisabled ? null : drawer}
            {isImageAttachEnabled || isFileAttachEnabled || isDropEnabled ? (
              <ImageAttachmentsDrawer
                images={attachments.images}
                notice={attachments.notice}
                onRemove={attachments.remove}
                files={fileAttachments.images}
                fileNotice={fileAttachments.notice}
                onRemoveFile={fileAttachments.remove}
              />
            ) : null}
            {isPreviewEnabled && isPreviewOpen ? <ComposerPreview value={value} /> : null}
          </>
        }
        headerActions={
          /*
            One row, one gap: the icons from the prompt-library toggle to the
            preview toggle are one run, so no cluster boundary is left showing as
            a wider space.
          */
          <HStack align="center" gap={1}>
            <HStack align="center" gap={1}>
              <IconButton
                data-panel-toggle=""
                label={`${isPromptLibraryOpen ? 'Hide' : 'Show'} prompt library`}
                icon={
                  <Icon icon={isPromptLibraryOpen ? 'chevronLeft' : 'chevronRight'} size="sm" />
                }
                variant={isPromptLibraryOpen ? 'secondary' : 'ghost'}
                size="sm"
                onClick={onTogglePromptLibrary}
              />
              {toolbarActions}
            </HStack>
            <HStack align="center" gap={1}>
              {/*
                The attachment buttons are plain toolbar icons like their
                neighbours: no surface sets them apart as a group. Each keeps its
                own picker and its own direct action, and each appears only while
                its own attach flag is on.
              */}
              {isImageAttachEnabled ? (
                <>
                  <IconButton
                    label="Attach image"
                    isDisabled={isDisabled}
                    icon={<Icon icon={ImageIcon} size="sm" />}
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
              {isFileAttachEnabled ? (
                <>
                  <IconButton
                    label="Attach file"
                    isDisabled={isDisabled}
                    icon={<Icon icon={FileIcon} size="sm" />}
                    variant="ghost"
                    size="sm"
                    onClick={() => openFilePickerRef.current?.()}
                  />
                  <FileAttachmentPicker
                    accept={appConfig.attach_file_accept}
                    onFiles={files => void fileAttachments.add(files)}
                    openRef={openFilePickerRef}
                  />
                </>
              ) : null}
              {isPreviewEnabled ? (
                <IconButton
                  label={isPreviewOpen ? 'Hide markdown preview' : 'Preview markdown'}
                  isDisabled={isDisabled}
                  icon={<Icon icon={PreviewIcon} size="sm" />}
                  variant={isPreviewOpen ? 'secondary' : 'ghost'}
                  size="sm"
                  onClick={() => setIsPreviewOpen(current => !current)}
                />
              ) : null}
              {isPlanModeEnabled ? (
                <IconButton
                  label={isPlan ? 'Turn off plan mode' : 'Plan mode: read-only tools, the answer is a plan'}
                  isDisabled={isDisabled}
                  icon={<Icon icon={PlanIcon} size="sm" />}
                  variant={isPlan ? 'secondary' : 'ghost'}
                  size="sm"
                  onClick={togglePlan}
                />
              ) : null}
              {isPlan && !isRunning ? (
                <Button
                  label={appConfig.plan_mode_execute_label}
                  isDisabled={isDisabled}
                  variant="secondary"
                  size="sm"
                  onClick={executePlan}
                />
              ) : null}
              {pendingCommand === undefined ? null : (
                <Token
                  size="sm"
                  label={commandOptionsLabel(pendingCommand)}
                  onRemove={clearPendingCommand}
                />
              )}
            </HStack>
          </HStack>
        }
        headerContext={
          <HStack align="center" gap={1}>
            {headerContextActions}
            <IconButton
              data-panel-toggle=""
              label={showInspector ? 'Hide run inspector' : 'Show run inspector'}
              icon={<Icon icon="viewColumns" size="sm" />}
              variant={showInspector ? 'secondary' : 'ghost'}
              size="sm"
              onClick={onToggleInspector}
            />
          </HStack>
        }
        footerActions={footerNote}
        elevation="low"
      />
    </>
  );
}
