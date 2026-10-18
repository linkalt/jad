/**
 * @file image-attachments.ts
 * Images a user attaches to a message: which files are taken, how they are read,
 * and the run input they become. Pure functions, so the rules are tested without
 * a browser. The server has the last word (it checks the bytes); these limits only
 * keep an obviously refused request from being sent.
 */

export interface ImageAttachmentLimits {
  /** Most images one message may carry. */
  maxCount: number;
  /** Largest single file, in bytes. */
  maxBytes: number;
  /** Accepted MIME types, lower case. */
  accept: readonly string[];
}

/** One attached image: a data URL the composer shows and the run request carries. */
export interface AttachedImage {
  id: string;
  name: string;
  dataUrl: string;
}

/** One attached file (a PDF): its name and a data URL the run request carries. */
export type AttachedFile = AttachedImage;

export interface RejectedImage {
  name: string;
  reason: string;
}

export interface ImageFilePlan {
  accepted: File[];
  rejected: RejectedImage[];
}

/** One part of a structured run input, as `POST /sessions/{s}/run` takes it. */
export type RunInputPart =
  | {type: 'text'; text: string}
  | {type: 'image'; data: string}
  | {type: 'file'; name: string; data: string};

export type RunInput = string | RunInputPart[];

/** Parse the `accept` config string ("image/png,image/jpeg") into MIME types. */
export function parseAcceptList(accept: string): string[] {
  return accept
    .split(',')
    .map(entry => entry.trim().toLowerCase())
    .filter(entry => entry.length > 0);
}

export function formatBytes(bytes: number): string {
  return bytes >= 1024 * 1024
    ? `${(bytes / (1024 * 1024)).toFixed(1)} MiB`
    : `${Math.ceil(bytes / 1024)} KiB`;
}

/** The decoded size, in bytes, of the base64 body of a data URL. */
export function dataUrlBytes(dataUrl: string): number {
  const body = dataUrl.slice(dataUrl.indexOf(',') + 1);
  const padding = body.endsWith('==') ? 2 : body.endsWith('=') ? 1 : 0;
  return Math.max(0, Math.floor((body.length * 3) / 4) - padding);
}

/** What one kind of attachment accepts; the same rules serve images and files. */
export interface AttachmentRule {
  /** Whether the file is a type this kind takes. */
  accepts: (file: File) => boolean;
  /** Refusal reason for a file of another type. */
  badType: string;
  maxCount: number;
  maxBytes: number;
  /** Plural noun for the count refusal ("images", "files"). */
  plural: string;
  /** Name for a file that has none (a pasted one). */
  fallbackName: string;
}

/**
 * Decide which of the offered files become attachments. Files are taken in order
 * until the message holds `maxCount` of them; each refused file gets a reason.
 */
export function planAttachmentFiles(
  files: readonly File[],
  existingCount: number,
  rule: AttachmentRule,
): ImageFilePlan {
  const accepted: File[] = [];
  const rejected: RejectedImage[] = [];
  for (const file of files) {
    const name = file.name === '' ? rule.fallbackName : file.name;
    if (!rule.accepts(file)) {
      rejected.push({name, reason: rule.badType});
    } else if (file.size === 0) {
      rejected.push({name, reason: 'the file is empty'});
    } else if (file.size > rule.maxBytes) {
      rejected.push({name, reason: `larger than ${formatBytes(rule.maxBytes)}`});
    } else if (existingCount + accepted.length >= rule.maxCount) {
      rejected.push({name, reason: `a message holds at most ${rule.maxCount} ${rule.plural}`});
    } else {
      accepted.push(file);
    }
  }
  return {accepted, rejected};
}

/** The rule for images: the MIME type must be on the accept list. */
export function imageRule(limits: ImageAttachmentLimits): AttachmentRule {
  return {
    accepts: file => limits.accept.includes(file.type.toLowerCase()),
    badType: 'not a supported image type',
    maxCount: limits.maxCount,
    maxBytes: limits.maxBytes,
    plural: 'images',
    fallbackName: 'pasted image',
  };
}

/**
 * Decide which of the offered files become image attachments (see
 * `planAttachmentFiles`).
 */
export function planImageFiles(
  files: readonly File[],
  existingCount: number,
  limits: ImageAttachmentLimits,
): ImageFilePlan {
  return planAttachmentFiles(files, existingCount, imageRule(limits));
}

const BASE64_CHUNK_BYTES = 0x8000;

/** Read a file as a `data:` URL. Works in a browser and in Node, with no FileReader. */
export async function readImageAsDataUrl(file: Blob): Promise<string> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  let binary = '';
  for (let offset = 0; offset < bytes.length; offset += BASE64_CHUNK_BYTES) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + BASE64_CHUNK_BYTES));
  }
  return `data:${file.type || 'application/octet-stream'};base64,${btoa(binary)}`;
}

/**
 * The run input for a message: a plain string without attachments, parts with
 * them (the text first, then the images, then the files).
 */
export function buildRunInput(
  text: string,
  images: readonly {dataUrl: string}[],
  files: readonly {name: string; dataUrl: string}[] = [],
): RunInput {
  if (images.length === 0 && files.length === 0) return text;
  return [
    {type: 'text', text},
    ...images.map((image): RunInputPart => ({type: 'image', data: image.dataUrl})),
    ...files.map((file): RunInputPart => ({type: 'file', name: file.name, data: file.dataUrl})),
  ];
}

/** What a user message shows: its images and files, and the text left once the placeholders are hidden. */
export interface UserMessageImages {
  /** Data URLs of images this tab just sent. */
  sent: string[];
  /** Stored image file names the server listed on the message. */
  stored: string[];
  /** Names of the files attached to the message, in order (those this tab just sent, or the server's list). */
  files: string[];
  text: string;
}

// The server's stand-in for the text of an image-only message (see webui.UserContent).
const IMAGE_PLACEHOLDER = /^\[image\](\s\[image\])*$/;

/**
 * Split a user message into what to draw. The server's `[image]` and `[file: name]`
 * placeholders are dropped only when there is something to draw in their place; an
 * image that the server holds inline cannot be fetched, so its placeholder stays visible.
 */
export function userMessageImages(message: {
  text: string;
  sentImages?: readonly string[];
  sentFiles?: readonly string[];
  parts?: readonly {type: string; images?: readonly string[]; files?: readonly string[]}[];
}): UserMessageImages {
  const sent = [...(message.sentImages ?? [])];
  const stored: string[] = [];
  const serverFiles: string[] = [];
  for (const part of message.parts ?? []) {
    if (part.type !== 'text') continue;
    for (const name of part.images ?? []) {
      if (!stored.includes(name)) stored.push(name);
    }
    serverFiles.push(...(part.files ?? []));
  }
  const files = message.sentFiles !== undefined ? [...message.sentFiles] : serverFiles;
  const hasImages = sent.length > 0 || stored.length > 0;
  const trimmed = message.text.trim();
  // A message with a file and no text of its own shows "[file: name]" for each file; with
  // text of its own it shows only the text. Strip the placeholders of the known files and
  // see what is left: nothing, or only image placeholders, means there was no text.
  let rest = trimmed;
  for (const name of files) rest = rest.replace(`[file: ${name}]`, '');
  rest = rest.trim();
  const isPlaceholderOnly = rest === '' || IMAGE_PLACEHOLDER.test(rest);
  let text = message.text;
  if (files.length > 0 && isPlaceholderOnly) {
    text = hasImages ? '' : rest;
  } else if (hasImages && files.length === 0 && IMAGE_PLACEHOLDER.test(trimmed)) {
    text = '';
  }
  return {sent, stored, files, text};
}

/** One line for the composer: what was refused and why. Empty when nothing was. */
export function describeRejected(rejected: readonly RejectedImage[]): string {
  if (rejected.length === 0) return '';
  return rejected.map(item => `${item.name}: ${item.reason}`).join('; ');
}

/**
 * How many images each draft holds or is about to hold, kept outside React state so
 * two quick adds see each other at once. A count read from rendered state is stale
 * for a moment after every add, which let two overlapping offers pass the limit.
 */
export class ImageSlots {
  private readonly counts = new Map<string, number>();
  private readonly generations = new Map<string, number>();

  count(draftKey: string): number {
    return this.counts.get(draftKey) ?? 0;
  }

  /** Changes every time the draft is sent; an add started under an older value is stale. */
  generation(draftKey: string): number {
    return this.generations.get(draftKey) ?? 0;
  }

  /** Take `n` places for images that are being read. */
  reserve(draftKey: string, n: number): void {
    this.counts.set(draftKey, this.count(draftKey) + n);
  }

  /** Give `n` places back: an image that could not be read, or one the user removed. */
  release(draftKey: string, n: number): void {
    this.counts.set(draftKey, Math.max(0, this.count(draftKey) - n));
  }

  /** The draft was sent: its places are free again and adds still in flight become stale. */
  clear(draftKey: string): void {
    this.counts.delete(draftKey);
    this.generations.set(draftKey, this.generation(draftKey) + 1);
  }
}

export interface AddAttachmentsDeps {
  draftKey: string;
  /** `slots.generation(draftKey)` when the user offered the files, before any waiting. */
  generation: number;
  /** Which files this kind takes, and its limits. */
  rule: AttachmentRule;
  slots: ImageSlots;
  /** Makes the id of one new attachment; ids are unique per hook. */
  makeId: () => string;
  /** Reads one file; replaceable in tests. */
  read?: (file: File) => Promise<string>;
  /** Called with each attachment as soon as it is read, in order. `file` is the source file. */
  commit: (attachment: AttachedImage, file: File) => void;
  /** Called with the refusal text (empty when nothing was refused). */
  notify: (notice: string) => void;
  /** Refusals found before the files got here (a dropped file of another kind). */
  extraRefused?: readonly RejectedImage[];
}

export interface AddImagesDeps extends Omit<AddAttachmentsDeps, 'rule'> {
  limits: ImageAttachmentLimits;
}

const SENT_FIRST = 'was still loading when the message was sent; attach it again';

/**
 * Take files for a draft: refuse what does not fit, read the rest, hand them on.
 * Places are reserved before the first await, so a second call that starts while
 * this one is still reading counts these files already. If the draft is sent
 * before a file is ready, that file is dropped with a notice instead of landing in
 * the next message.
 */
export async function addAttachmentFiles(
  files: readonly File[],
  deps: AddAttachmentsDeps,
): Promise<void> {
  const {draftKey, generation, rule, slots, makeId, commit, notify} = deps;
  const read = deps.read ?? readImageAsDataUrl;
  const extra = deps.extraRefused ?? [];
  const label = (file: File) => (file.name === '' ? rule.fallbackName : file.name);
  const isStale = () => slots.generation(draftKey) !== generation;
  if (isStale()) {
    notify(
      describeRejected([
        ...extra,
        ...files.map(file => ({name: label(file), reason: SENT_FIRST})),
      ]),
    );
    return;
  }
  const plan = planAttachmentFiles(files, slots.count(draftKey), rule);
  slots.reserve(draftKey, plan.accepted.length);
  const refused = [...extra, ...plan.rejected];
  for (const file of plan.accepted) {
    const name = label(file);
    let dataUrl: string;
    try {
      dataUrl = await read(file);
    } catch {
      if (!isStale()) slots.release(draftKey, 1);
      refused.push({name, reason: 'could not be read'});
      continue;
    }
    if (isStale()) {
      refused.push({name, reason: SENT_FIRST});
      continue;
    }
    commit({id: makeId(), name, dataUrl}, file);
  }
  notify(describeRejected(refused));
}

/** `addAttachmentFiles` with the image rule. */
export function addImageFiles(files: readonly File[], deps: AddImagesDeps): Promise<void> {
  const {limits, ...rest} = deps;
  return addAttachmentFiles(files, {...rest, rule: imageRule(limits)});
}

/**
 * Run async tasks one after another, in the order they were started. Image reads
 * take different times, so without this a small file offered later could finish
 * and appear before a larger one offered earlier.
 */
export function createSerialQueue(): (task: () => Promise<void>) => Promise<void> {
  let tail: Promise<void> = Promise.resolve();
  return task => {
    const run = tail.then(task);
    tail = run.catch(() => undefined);
    return run;
  };
}
