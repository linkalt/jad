/**
 * @file FileAttachments.tsx
 * Files (PDF) attached to a message: the icon and the chips that show their names,
 * in the composer drawer before a send and under the user's message after it, and the
 * hidden input behind the attach-file button. The bytes are never drawn.
 */
import {useEffect, useRef} from 'react';
import type {ChangeEvent, SVGProps} from 'react';
import {HStack} from '@astryxdesign/core';
import {Icon} from '@astryxdesign/core/Icon';
import {Token} from '@astryxdesign/core/Token';

/** A sheet of paper with a folded corner; Astryx has no semantic icon for a file. */
export function FileIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5} {...props}>
      <path
        d="M14 3H7.5A1.5 1.5 0 0 0 6 4.5v15A1.5 1.5 0 0 0 7.5 21h9a1.5 1.5 0 0 0 1.5-1.5V7Z"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <path d="M14 3v4h4" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export interface FileChip {
  /** Unique within the row; the file's own id before a send, its index after. */
  id: string;
  name: string;
  /** Shown after the name, for example the size. */
  detail?: string | undefined;
}

export interface FileChipsProps {
  names: readonly FileChip[];
  /** Shows a remove button on each chip when given. */
  onRemove?: ((id: string) => void) | undefined;
  /** Right-aligns the chips (under the user's message). */
  isEnd?: boolean;
}

/** One chip per file, with the file's name; a remove button before the message is sent. */
export function FileChips({names, onRemove, isEnd = false}: FileChipsProps) {
  if (names.length === 0) return null;
  return (
    <HStack gap={2} wrap="wrap" justify={isEnd ? 'end' : 'start'}>
      {names.map(chip => (
        <Token
          key={chip.id}
          label={chip.detail ? `${chip.name} · ${chip.detail}` : chip.name}
          size="md"
          icon={<Icon icon={FileIcon} size="sm" />}
          onRemove={onRemove === undefined ? undefined : () => onRemove(chip.id)}
        />
      ))}
    </HStack>
  );
}

/** Chips for the names of files already sent: the same file name may appear twice. */
export function SentFiles({names}: {names: readonly string[]}) {
  return (
    <FileChips names={names.map((name, index) => ({id: `${index}-${name}`, name}))} isEnd />
  );
}

export interface FileAttachmentPickerProps {
  accept: string;
  onFiles: (files: File[]) => void;
  /** Hands the opener to the parent so a toolbar button can open the picker. */
  openRef: {current: (() => void) | null};
}

/** The hidden file input behind the attach-file button. */
export function FileAttachmentPicker({accept, onFiles, openRef}: FileAttachmentPickerProps) {
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
