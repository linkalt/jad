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

export interface RejectedImage {
  name: string;
  reason: string;
}

export interface ImageFilePlan {
  accepted: File[];
  rejected: RejectedImage[];
}

/** One part of a structured run input, as `POST /sessions/{s}/run` takes it. */
export type RunInputPart = {type: 'text'; text: string} | {type: 'image'; data: string};

export type RunInput = string | RunInputPart[];

/** Parse the `accept` config string ("image/png,image/jpeg") into MIME types. */
export function parseAcceptList(accept: string): string[] {
  return accept
    .split(',')
    .map(entry => entry.trim().toLowerCase())
    .filter(entry => entry.length > 0);
}

function formatBytes(bytes: number): string {
  return bytes >= 1024 * 1024
    ? `${(bytes / (1024 * 1024)).toFixed(1)} MiB`
    : `${Math.ceil(bytes / 1024)} KiB`;
}

/**
 * Decide which of the offered files become attachments. Files are taken in order
 * until the message holds `maxCount` images; each refused file gets a reason.
 */
export function planImageFiles(
  files: readonly File[],
  existingCount: number,
  limits: ImageAttachmentLimits,
): ImageFilePlan {
  const accepted: File[] = [];
  const rejected: RejectedImage[] = [];
  for (const file of files) {
    const name = file.name === '' ? 'pasted image' : file.name;
    if (!limits.accept.includes(file.type.toLowerCase())) {
      rejected.push({name, reason: 'not a supported image type'});
    } else if (file.size === 0) {
      rejected.push({name, reason: 'the file is empty'});
    } else if (file.size > limits.maxBytes) {
      rejected.push({name, reason: `larger than ${formatBytes(limits.maxBytes)}`});
    } else if (existingCount + accepted.length >= limits.maxCount) {
      rejected.push({name, reason: `a message holds at most ${limits.maxCount} images`});
    } else {
      accepted.push(file);
    }
  }
  return {accepted, rejected};
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

/** The run input for a message: a plain string without images, parts with them. */
export function buildRunInput(text: string, images: readonly {dataUrl: string}[]): RunInput {
  if (images.length === 0) return text;
  return [
    {type: 'text', text},
    ...images.map((image): RunInputPart => ({type: 'image', data: image.dataUrl})),
  ];
}

/** What a user message shows: its images, and the text left once an image placeholder is hidden. */
export interface UserMessageImages {
  /** Data URLs of images this tab just sent. */
  sent: string[];
  /** Stored image file names the server listed on the message. */
  stored: string[];
  text: string;
}

// The server's stand-in for the text of an image-only message (see webui.UserContent).
const IMAGE_PLACEHOLDER = /^\[image\](\s\[image\])*$/;

/**
 * Split a user message into what to draw. The server's `[image]` placeholder is
 * dropped only when there is an image to draw in its place; an image that the
 * server holds inline cannot be fetched, so its placeholder stays visible.
 */
export function userMessageImages(message: {
  text: string;
  sentImages?: readonly string[];
  parts?: readonly {type: string; images?: readonly string[]}[];
}): UserMessageImages {
  const sent = [...(message.sentImages ?? [])];
  const stored: string[] = [];
  for (const part of message.parts ?? []) {
    if (part.type !== 'text') continue;
    for (const name of part.images ?? []) {
      if (!stored.includes(name)) stored.push(name);
    }
  }
  const hasImages = sent.length > 0 || stored.length > 0;
  const text = hasImages && IMAGE_PLACEHOLDER.test(message.text.trim()) ? '' : message.text;
  return {sent, stored, text};
}

/** One line for the composer: what was refused and why. Empty when nothing was. */
export function describeRejected(rejected: readonly RejectedImage[]): string {
  if (rejected.length === 0) return '';
  return rejected.map(item => `${item.name}: ${item.reason}`).join('; ');
}
