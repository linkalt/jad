/**
 * @file StoredImages.tsx
 * @input File names of images the server stored for one tool result, or the position of images it still holds inline, and the session they belong to
 * @output A row of thumbnails; a click opens the images in a lightbox
 * @position Rendered under the text of a tool result that carries `images` or `inlineImages`
 *
 * The image route needs the API token, so each image is fetched through the
 * API client and shown from an object URL that is revoked when the row goes away.
 * An image that cannot be loaded shows as a plain line, never as a broken picture.
 */
import {createContext, useCallback, useContext, useEffect, useMemo, useState} from 'react';
import {HStack, Text, VStack} from '@astryxdesign/core';
import {Lightbox} from '@astryxdesign/core/Lightbox';
import {Thumbnail} from '@astryxdesign/core/Thumbnail';

import {fetchMessageImage, fetchSessionImage} from '../api';

/** Where stored images are read from: the session on screen and its profile. */
export interface StoredImageScope {
  sessionRef: string;
  profile?: string;
}

const StoredImageScopeContext = createContext<StoredImageScope | undefined>(undefined);

export const StoredImageScopeProvider = StoredImageScopeContext.Provider;

type LoadedImage =
  | {name: string; state: 'loading'}
  | {name: string; state: 'ready'; url: string}
  | {name: string; state: 'failed'};

export interface StoredImagesProps {
  names: readonly string[];
}

export function StoredImages({names}: StoredImagesProps) {
  const scope = useContext(StoredImageScopeContext);
  const sessionRef = scope?.sessionRef;
  const profile = scope?.profile;
  const load = useCallback(
    (name: string, signal: AbortSignal) =>
      sessionRef === undefined
        ? Promise.reject(new Error('no session'))
        : fetchSessionImage(sessionRef, name, profile, signal),
    [sessionRef, profile],
  );
  return <ImageRow ids={names} load={load} noun="Stored image" />;
}

export interface InlineImagesProps {
  messageId: string;
  callId: string;
  /** How many images the result still holds inline, from `inlineImages`. */
  count: number;
  /** Turn of the message. A message of the history block has none; the server then searches that block. */
  cursor?: number;
}

/** Images a tool result still holds inline in the transcript; the server decodes each on demand. */
export function InlineImages({messageId, callId, count, cursor}: InlineImagesProps) {
  const scope = useContext(StoredImageScopeContext);
  const sessionRef = scope?.sessionRef;
  const profile = scope?.profile;
  const ids = useMemo(() => Array.from({length: count}, (_, index) => String(index)), [count]);
  const load = useCallback(
    (position: string, signal: AbortSignal) =>
      sessionRef === undefined
        ? Promise.reject(new Error('no session'))
        : fetchMessageImage(sessionRef, messageId, callId, Number(position), cursor, profile, signal),
    [sessionRef, messageId, callId, cursor, profile],
  );
  return <ImageRow ids={ids} load={load} noun="Image" />;
}

interface ImageRowProps {
  /** One id per image, passed back to `load`. */
  ids: readonly string[];
  load: (id: string, signal: AbortSignal) => Promise<Blob>;
  /** Label prefix for a thumbnail and its lightbox entry. */
  noun: string;
}

function ImageRow({ids, load, noun}: ImageRowProps) {
  const scope = useContext(StoredImageScopeContext);
  const [images, setImages] = useState<LoadedImage[]>([]);
  const [openIndex, setOpenIndex] = useState<number | undefined>(undefined);
  const key = ids.join(',');
  const hasScope = scope !== undefined;

  useEffect(() => {
    if (!hasScope || key.length === 0) {
      setImages([]);
      return undefined;
    }
    const wanted = key.split(',');
    const controller = new AbortController();
    const urls: string[] = [];
    setImages(wanted.map(name => ({name, state: 'loading'})));
    wanted.forEach((name, index) => {
      load(name, controller.signal)
        .then(blob => {
          if (controller.signal.aborted) return;
          const url = URL.createObjectURL(blob);
          urls.push(url);
          setImages(current => current.map((item, at) => (at === index ? {name, state: 'ready', url} : item)));
        })
        .catch(() => {
          if (controller.signal.aborted) return;
          setImages(current => current.map((item, at) => (at === index ? {name, state: 'failed'} : item)));
        });
    });
    return () => {
      controller.abort();
      urls.forEach(url => URL.revokeObjectURL(url));
    };
  }, [key, hasScope, load]);

  if (ids.length === 0 || !hasScope) {
    return null;
  }
  const ready = images.flatMap(image => (image.state === 'ready' ? [image] : []));
  return (
    <VStack gap={1} width="100%">
      <HStack gap={2} wrap="wrap">
        {images.map(image => {
          if (image.state === 'ready') {
            const position = ready.findIndex(item => item.name === image.name);
            return (
              <Thumbnail
                key={image.name}
                src={image.url}
                label={`${noun} ${position + 1}`}
                onClick={() => setOpenIndex(position)}
              />
            );
          }
          return (
            <Text key={image.name} type="supporting" color="secondary">
              {image.state === 'loading' ? 'Loading image…' : 'Image unavailable'}
            </Text>
          );
        })}
      </HStack>
      {ready.length > 0 && (
        <Lightbox
          isOpen={openIndex !== undefined}
          onOpenChange={isOpen => {
            if (!isOpen) setOpenIndex(undefined);
          }}
          media={ready.map((image, index) => ({src: image.url, alt: `${noun} ${index + 1}`}))}
          index={openIndex ?? 0}
          onIndexChange={setOpenIndex}
          hasZoom
        />
      )}
    </VStack>
  );
}
