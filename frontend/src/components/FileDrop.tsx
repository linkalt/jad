/**
 * @file FileDrop.tsx
 * Files dropped or pasted into the composer. Images join the image attachments, PDF
 * files join the file attachments, and everything else is refused with a reason
 * shown in the attachments drawer. Nothing here talks to the server.
 */
import {useCallback, useRef, useState} from 'react';
import type {DragEvent} from 'react';

import {classifyFiles, dragHasFiles} from '../file-intake';
import type {FileIntakeRules} from '../file-intake';
import type {RejectedImage} from '../image-attachments';

export interface FileDropOptions {
  rules: FileIntakeRules;
  /** Hands images, and the refusals to show, to the image attachments. */
  addImages: (files: readonly File[], refused: readonly RejectedImage[]) => Promise<void>;
  /** Hands files to the file attachments. */
  addFiles: (files: readonly File[]) => Promise<void>;
}

export interface FileDrop {
  /** True while a drag that carries files is over the composer. */
  isDragging: boolean;
  /** Take files from a drop or a paste. */
  handleFiles: (files: File[]) => void;
  /** Props for the element that is the drop target. */
  targetProps: {
    onDragEnter: (event: DragEvent<HTMLElement>) => void;
    onDragOver: (event: DragEvent<HTMLElement>) => void;
    onDragLeave: (event: DragEvent<HTMLElement>) => void;
    onDrop: (event: DragEvent<HTMLElement>) => void;
  };
}

export function useFileDrop(options: FileDropOptions): FileDrop {
  const {rules, addImages, addFiles} = options;
  const [isDragging, setIsDragging] = useState(false);
  // dragenter/dragleave fire for every child the pointer crosses; a depth count
  // keeps the highlight steady until the pointer really leaves the target.
  const depthRef = useRef(0);

  const handleFiles = useCallback(
    (files: File[]) => {
      const plan = classifyFiles(files, rules);
      // Always called, even with no images: it is also how a refusal is shown.
      void addImages(plan.images, plan.refused);
      if (plan.files.length > 0) void addFiles(plan.files);
    },
    [addFiles, addImages, rules],
  );

  const onDragEnter = useCallback((event: DragEvent<HTMLElement>) => {
    if (!dragHasFiles(event.dataTransfer?.types)) return;
    depthRef.current += 1;
    setIsDragging(true);
  }, []);

  const onDragOver = useCallback((event: DragEvent<HTMLElement>) => {
    // Without this the browser would open the file instead of dropping it.
    if (dragHasFiles(event.dataTransfer?.types)) event.preventDefault();
  }, []);

  const onDragLeave = useCallback((event: DragEvent<HTMLElement>) => {
    if (!dragHasFiles(event.dataTransfer?.types)) return;
    depthRef.current = Math.max(0, depthRef.current - 1);
    // A child removed during the drag never sends its leave: when the pointer went to a
    // node outside the pane, the drag is over whatever the count says.
    const next = event.relatedTarget;
    if (next instanceof Node && !event.currentTarget.contains(next)) depthRef.current = 0;
    if (depthRef.current === 0) setIsDragging(false);
  }, []);

  const onDrop = useCallback(
    (event: DragEvent<HTMLElement>) => {
      depthRef.current = 0;
      setIsDragging(false);
      // The composer's input takes drops on itself and marks them handled.
      if (event.defaultPrevented) return;
      const files = Array.from(event.dataTransfer?.files ?? []);
      if (files.length === 0) return;
      event.preventDefault();
      handleFiles(files);
    },
    [handleFiles],
  );

  return {
    isDragging,
    handleFiles,
    targetProps: {onDragEnter, onDragOver, onDragLeave, onDrop},
  };
}
