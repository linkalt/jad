/**
 * @file resource-attachments.ts
 * @input The images and PDFs an MCP resource read returned (base64)
 * @output File objects the composer's attachment lists take, as a dropped file would be
 * @position Between the resource read in api.ts and the composer's attachments
 */

export interface MCPResourceAttachment {
  /** "image" (PNG, JPEG, GIF, WebP) or "file" (PDF). */
  type: 'image' | 'file';
  uri: string;
  mimeType: string;
  /** A file's name; absent for an image. */
  name?: string;
  /** The content, base64. */
  data: string;
}

const IMAGE_EXTENSIONS: Record<string, string> = {
  'image/png': 'png',
  'image/jpeg': 'jpg',
  'image/gif': 'gif',
  'image/webp': 'webp',
};

/** The file name an attachment is shown with: its own, else the last part of its URI. */
export function resourceAttachmentName(attachment: MCPResourceAttachment, fallback: string): string {
  if (attachment.name !== undefined && attachment.name !== '') return attachment.name;
  const path = attachment.uri.replace(/^[A-Za-z][A-Za-z0-9+.-]*:\/*/, '');
  const tail = path.split(/[/:]/).filter(part => part !== '').pop()?.split('?')[0] ?? '';
  const base = tail.replace(/[^A-Za-z0-9._-]+/g, '_') || fallback;
  const extension = IMAGE_EXTENSIONS[attachment.mimeType];
  return extension === undefined || /\.[A-Za-z0-9]+$/.test(base) ? base : `${base}.${extension}`;
}

/** Splits a resource's attachments into image and file Files; `fallback` names an image whose URI has no name. */
export function resourceAttachmentFiles(
  attachments: readonly MCPResourceAttachment[],
  fallback: string,
): {images: File[]; files: File[]} {
  const images: File[] = [];
  const files: File[] = [];
  for (const attachment of attachments) {
    const binary = atob(attachment.data);
    const bytes = new Uint8Array(binary.length);
    for (let index = 0; index < binary.length; index++) bytes[index] = binary.charCodeAt(index);
    const file = new File([bytes], resourceAttachmentName(attachment, fallback), {type: attachment.mimeType});
    (attachment.type === 'image' ? images : files).push(file);
  }
  return {images, files};
}

/** Reads the attachments member of a resource read; null when it is malformed. */
export function parseResourceAttachments(value: unknown): MCPResourceAttachment[] | null {
  if (value === undefined) return [];
  if (!Array.isArray(value)) return null;
  const parsed: MCPResourceAttachment[] = [];
  for (const item of value) {
    if (
      typeof item !== 'object' || item === null ||
      (item.type !== 'image' && item.type !== 'file') ||
      typeof item.uri !== 'string' || typeof item.mime_type !== 'string' || typeof item.data !== 'string' ||
      (item.name !== undefined && typeof item.name !== 'string')
    ) {
      return null;
    }
    parsed.push({
      type: item.type,
      uri: item.uri,
      mimeType: item.mime_type,
      data: item.data,
      ...(typeof item.name === 'string' ? {name: item.name} : {}),
    });
  }
  return parsed;
}
