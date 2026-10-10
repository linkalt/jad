/**
 * @file file-intake.ts
 * Files that arrive in the composer by drop or paste: which become image
 * attachments, which become file attachments (PDF), which are refused and why.
 * Pure functions, so the rules are tested without a browser. The count and size
 * limits stay with `planAttachmentFiles`; this only sorts the files.
 */
import type {AttachmentRule, RejectedImage} from './image-attachments';

export interface FileIntakeRules {
  /** Accepted image MIME types, lower case. */
  imageAccept: readonly string[];
  /** Accepted file MIME types, lower case ("application/pdf"). */
  fileAccept: readonly string[];
  /** Accepted file extensions, lower case with the dot (".pdf"). */
  fileExtensions: readonly string[];
  isImageEnabled: boolean;
  isFileEnabled: boolean;
}

export interface FileIntakePlan {
  images: File[];
  files: File[];
  refused: RejectedImage[];
}

/** The dotted extensions in an `accept` config string (".pdf,application/pdf"); other entries are ignored. */
export function parseExtensionList(accept: string): string[] {
  return accept
    .split(',')
    .map(entry => entry.trim().toLowerCase())
    .filter(entry => entry.length > 1 && entry.startsWith('.'));
}

/** The MIME types in an `accept` config string; dotted extensions are ignored. */
export function parseMimeList(accept: string): string[] {
  return accept
    .split(',')
    .map(entry => entry.trim().toLowerCase())
    .filter(entry => entry.includes('/'));
}

/** True when the file is on the accept list by its MIME type or, failing that, its extension. */
export function isAcceptedFile(
  file: Pick<File, 'name' | 'type'>,
  mimeTypes: readonly string[],
  extensions: readonly string[],
): boolean {
  if (mimeTypes.includes(file.type.toLowerCase())) return true;
  const name = file.name.toLowerCase();
  return extensions.some(extension => name.endsWith(extension));
}

/** Sort offered files into images, files and refusals, keeping their order. */
export function classifyFiles(files: readonly File[], rules: FileIntakeRules): FileIntakePlan {
  const images: File[] = [];
  const accepted: File[] = [];
  const refused: RejectedImage[] = [];
  for (const file of files) {
    const name = file.name === '' ? 'pasted file' : file.name;
    if (rules.imageAccept.includes(file.type.toLowerCase())) {
      if (rules.isImageEnabled) images.push(file);
      else refused.push({name, reason: 'image attachments are off'});
    } else if (isAcceptedFile(file, rules.fileAccept, rules.fileExtensions)) {
      if (rules.isFileEnabled) accepted.push(file);
      else refused.push({name, reason: 'file attachments are off'});
    } else {
      refused.push({name, reason: 'not a supported image or file type'});
    }
  }
  return {images, files: accepted, refused};
}

/** The attachment rule for files: type by MIME or extension, with the file limits. */
export function fileRule(limits: {
  maxCount: number;
  maxBytes: number;
  mimeTypes: readonly string[];
  extensions: readonly string[];
}): AttachmentRule {
  return {
    accepts: file => isAcceptedFile(file, limits.mimeTypes, limits.extensions),
    badType: 'not a supported file type',
    maxCount: limits.maxCount,
    maxBytes: limits.maxBytes,
    plural: 'files',
    fallbackName: 'pasted file',
  };
}

/** True when a drag carries files (and not, say, selected text being moved). */
export function dragHasFiles(types: ArrayLike<string> | null | undefined): boolean {
  return Array.from(types ?? []).includes('Files');
}
