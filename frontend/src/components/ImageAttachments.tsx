/**
 * @file ImageAttachments.tsx
 * The composer's image attachments: the hook that holds them per draft, the drawer
 * that shows them above the input, and a small gallery for images that are still
 * data URLs (a message just sent, before the server's copy arrives).
 */
import {useCallback, useRef, useState} from 'react';
import type {ChangeEvent} from 'react';
import {HStack, Text, VStack} from '@astryxdesign/core';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';
import {Lightbox} from '@astryxdesign/core/Lightbox';
import {Thumbnail} from '@astryxdesign/core/Thumbnail';

import {
  describeRejected,
  planImageFiles,
  readImageAsDataUrl,
} from '../image-attachments';
import type {AttachedImage, ImageAttachmentLimits} from '../image-attachments';

export interface ImageAttachments {
  /** Images of the draft on screen, in the order they will be sent. */
  images: AttachedImage[];
  /** Why the last offer was partly or wholly refused; empty when nothing was. */
  notice: string;
  /** Take files from a picker, a paste or a drop. */
  add: (files: readonly File[]) => Promise<void>;
  remove: (id: string) => void;
  /** Forget the images of the draft on screen (after a send). */
  clear: () => void;
}

/**
 * Images wait in memory only: a data URL is too large for the draft store in
 * localStorage, and a reload simply drops them. They are kept per draft key, so
 * switching rows keeps each row's attachments apart.
 */
export function useImageAttachments(
  draftKey: string,
  limits: ImageAttachmentLimits,
): ImageAttachments {
  const [byDraft, setByDraft] = useState<Record<string, AttachedImage[]>>({});
  const [notices, setNotices] = useState<Record<string, string>>({});
  const nextId = useRef(0);
  const imagesRef = useRef(byDraft);
  imagesRef.current = byDraft;

  const add = useCallback(
    async (files: readonly File[]) => {
      const existing = imagesRef.current[draftKey] ?? [];
      const plan = planImageFiles(files, existing.length, limits);
      setNotices(current => ({...current, [draftKey]: describeRejected(plan.rejected)}));
      const read: AttachedImage[] = [];
      for (const file of plan.accepted) {
        try {
          nextId.current += 1;
          read.push({
            id: `image-${nextId.current}`,
            name: file.name === '' ? 'pasted image' : file.name,
            dataUrl: await readImageAsDataUrl(file),
          });
        } catch {
          setNotices(current => ({...current, [draftKey]: `${file.name}: could not be read`}));
        }
      }
      if (read.length === 0) return;
      setByDraft(current => ({...current, [draftKey]: [...(current[draftKey] ?? []), ...read]}));
    },
    [draftKey, limits],
  );

  const remove = useCallback(
    (id: string) => {
      setNotices(current => ({...current, [draftKey]: ''}));
      setByDraft(current => ({
        ...current,
        [draftKey]: (current[draftKey] ?? []).filter(image => image.id !== id),
      }));
    },
    [draftKey],
  );

  const clear = useCallback(() => {
    setNotices(current => ({...current, [draftKey]: ''}));
    setByDraft(current => ({...current, [draftKey]: []}));
  }, [draftKey]);

  return {images: byDraft[draftKey] ?? [], notice: notices[draftKey] ?? '', add, remove, clear};
}

export interface ImageAttachmentPickerProps {
  accept: string;
  onFiles: (files: File[]) => void;
  /** Hands the opener to the parent so a toolbar button can open the picker. */
  openRef: {current: (() => void) | null};
}

/** The hidden file input behind the attach-image button. */
export function ImageAttachmentPicker({accept, onFiles, openRef}: ImageAttachmentPickerProps) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  openRef.current = () => inputRef.current?.click();
  const handleChange = (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    event.target.value = '';
    if (files.length > 0) onFiles(files);
  };
  return <input ref={inputRef} type="file" multiple accept={accept} onChange={handleChange} hidden />;
}

export interface ImageAttachmentsDrawerProps {
  images: readonly AttachedImage[];
  notice: string;
  onRemove: (id: string) => void;
}

/** Thumbnails of the attached images, each with a remove button, and any refusal notice. */
export function ImageAttachmentsDrawer({images, notice, onRemove}: ImageAttachmentsDrawerProps) {
  const [openIndex, setOpenIndex] = useState<number | undefined>(undefined);
  if (images.length === 0 && notice === '') return null;
  return (
    <ChatComposerDrawer>
      <VStack gap={1} width="100%">
        {images.length > 0 && (
          <HStack gap={2} wrap="wrap">
            {images.map((image, index) => (
              <Thumbnail
                key={image.id}
                src={image.dataUrl}
                alt={image.name}
                label={image.name}
                onClick={() => setOpenIndex(index)}
                onRemove={() => onRemove(image.id)}
                showRemoveOn="always"
              />
            ))}
          </HStack>
        )}
        {notice !== '' && (
          <Text type="supporting" color="secondary">
            {notice}
          </Text>
        )}
        {images.length > 0 && (
          <Lightbox
            isOpen={openIndex !== undefined}
            onOpenChange={isOpen => {
              if (!isOpen) setOpenIndex(undefined);
            }}
            media={images.map(image => ({src: image.dataUrl, alt: image.name}))}
            index={Math.min(openIndex ?? 0, images.length - 1)}
            onIndexChange={setOpenIndex}
            hasZoom
          />
        )}
      </VStack>
    </ChatComposerDrawer>
  );
}

/** Images of a message that was just sent, shown from their data URLs. */
export function SentImages({sources}: {sources: readonly string[]}) {
  const [openIndex, setOpenIndex] = useState<number | undefined>(undefined);
  if (sources.length === 0) return null;
  return (
    <VStack gap={1} width="100%">
      <HStack gap={2} wrap="wrap" justify="end">
        {sources.map((src, index) => (
          <Thumbnail
            key={`${index}-${src.length}`}
            src={src}
            alt={`Attached image ${index + 1}`}
            label="Open image"
            onClick={() => setOpenIndex(index)}
          />
        ))}
      </HStack>
      <Lightbox
        isOpen={openIndex !== undefined}
        onOpenChange={isOpen => {
          if (!isOpen) setOpenIndex(undefined);
        }}
        media={sources.map((src, index) => ({src, alt: `Attached image ${index + 1}`}))}
        index={openIndex ?? 0}
        onIndexChange={setOpenIndex}
        hasZoom
      />
    </VStack>
  );
}
