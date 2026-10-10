/**
 * @file ImageAttachments.tsx
 * The composer's image attachments: the hook that holds them per draft, the drawer
 * that shows them above the input, and a small gallery for images that are still
 * data URLs (a message just sent, before the server's copy arrives).
 */
import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import type {ChangeEvent} from 'react';
import {HStack, Text, VStack} from '@astryxdesign/core';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';
import {Lightbox} from '@astryxdesign/core/Lightbox';
import {Thumbnail} from '@astryxdesign/core/Thumbnail';

import {addImageFiles, createSerialQueue, ImageSlots} from '../image-attachments';
import type {AttachedImage, ImageAttachmentLimits} from '../image-attachments';

export interface ImageAttachments {
  /** Images of the draft on screen, in the order they will be sent. */
  images: AttachedImage[];
  /** Why the last offer was partly or wholly refused; empty when nothing was. */
  notice: string;
  /** Take files from a picker, a paste or a drop. */
  add: (files: readonly File[]) => Promise<void>;
  remove: (id: string) => void;
  /** Forget the images that were just sent. Images added after the send stay. */
  clear: (sent: readonly AttachedImage[]) => void;
}

/**
 * Images wait in memory only: a data URL is too large for the draft store in
 * localStorage, and a reload simply drops them. They are kept per draft key, so
 * switching rows keeps each row's attachments apart.
 *
 * A ref holds the truth and state only triggers the render, so a remove or a send
 * decides from what is there now and not from what the last render saw.
 */
export function useImageAttachments(
  draftKey: string,
  limits: ImageAttachmentLimits,
): ImageAttachments {
  const [, setVersion] = useState(0);
  const imagesRef = useRef<Record<string, AttachedImage[]>>({});
  const noticesRef = useRef<Record<string, string>>({});
  const nextId = useRef(0);
  const [slots] = useState(() => new ImageSlots());
  const [serial] = useState(() => createSerialQueue());
  const rerender = useCallback(() => setVersion(version => version + 1), []);

  const add = useCallback(
    (files: readonly File[]) => {
      // The generation is read now, before the queue makes this add wait: a send that
      // happens while it waits must make it stale.
      const generation = slots.generation(draftKey);
      return serial(() =>
        addImageFiles(files, {
          draftKey,
          generation,
          limits,
          slots,
          makeId: () => `image-${++nextId.current}`,
          commit: image => {
            imagesRef.current[draftKey] = [...(imagesRef.current[draftKey] ?? []), image];
            rerender();
          },
          notify: notice => {
            noticesRef.current[draftKey] = notice;
            rerender();
          },
        }),
      );
    },
    [draftKey, limits, rerender, serial, slots],
  );

  const remove = useCallback(
    (id: string) => {
      const current = imagesRef.current[draftKey] ?? [];
      // A remove for an image that is already gone must not free a place.
      if (!current.some(image => image.id === id)) return;
      slots.release(draftKey, 1);
      imagesRef.current[draftKey] = current.filter(image => image.id !== id);
      noticesRef.current[draftKey] = '';
      rerender();
    },
    [draftKey, rerender, slots],
  );

  const clear = useCallback(
    (sent: readonly AttachedImage[]) => {
      const sentIds = new Set(sent.map(image => image.id));
      const left = (imagesRef.current[draftKey] ?? []).filter(image => !sentIds.has(image.id));
      imagesRef.current[draftKey] = left;
      noticesRef.current[draftKey] = '';
      slots.clear(draftKey);
      // Images committed after the send was read stay in the draft and keep their places.
      if (left.length > 0) slots.reserve(draftKey, left.length);
      rerender();
    },
    [draftKey, rerender, slots],
  );

  const images = imagesRef.current[draftKey] ?? NO_IMAGES;
  const notice = noticesRef.current[draftKey] ?? '';
  return useMemo(
    () => ({images, notice, add, remove, clear}),
    [images, notice, add, remove, clear],
  );
}

const NO_IMAGES: AttachedImage[] = [];

export interface ImageAttachmentPickerProps {
  accept: string;
  onFiles: (files: File[]) => void;
  /** Hands the opener to the parent so a toolbar button can open the picker. */
  openRef: {current: (() => void) | null};
}

/** The hidden file input behind the attach-image button. */
export function ImageAttachmentPicker({accept, onFiles, openRef}: ImageAttachmentPickerProps) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    openRef.current = () => inputRef.current?.click();
    return () => {
      openRef.current = null;
    };
  }, [openRef]);
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
  // A lightbox opened on an image that is then removed must not reopen on the next one.
  useEffect(() => {
    if (images.length === 0) setOpenIndex(undefined);
  }, [images.length]);
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
                label={image.name}
                onClick={() => setOpenIndex(index)}
                onRemove={() => onRemove(image.id)}
                showRemoveOn="always"
              />
            ))}
          </HStack>
        )}
        {notice !== '' && (
          <Text type="supporting" role="status">
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
            label={`Attached image ${index + 1}`}
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
