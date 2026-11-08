import {useEffect} from 'react';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';
import {type ActivityDetail} from './activity-panel';
import {FloatingPanel} from './FloatingPanel';
import {HostedToolDetail, ReasoningDetail, ToolCallDetail} from './TranscriptParts';

/**
 * One reasoning or tool block of the transcript, in the pane's right-edge panel.
 * The body is the transcript's own rendering of the block, so the two cannot
 * drift; Escape closes it like the close button.
 */
export function ActivityPanel({
  detail,
  region,
  isEdgeFixed,
  onClose,
}: {
  detail: ActivityDetail;
  region: ResizableRegion;
  isEdgeFixed?: boolean;
  onClose: () => void;
}) {
  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && !event.defaultPrevented) {
        onClose();
      }
    };
    document.addEventListener('keydown', handleKeyDown);
    return () => document.removeEventListener('keydown', handleKeyDown);
  }, [onClose]);

  const title = detail.kind === 'reasoning' ? 'Reasoning' : detail.name;
  const subtitle = detail.kind === 'reasoning' ? (detail.isStreaming ? 'In progress' : undefined) : detail.target;
  return (
    <FloatingPanel
      side="end"
      layer="pane"
      label={detail.kind === 'reasoning' ? 'Reasoning' : 'Tool call'}
      title={title}
      subtitle={subtitle}
      region={region}
      isEdgeFixed={isEdgeFixed}
      onClose={onClose}
    >
      {detail.kind === 'reasoning' && <ReasoningDetail content={detail.content} steps={detail.steps} />}
      {detail.kind === 'tool' && <ToolCallDetail call={detail.call} result={detail.result} source={detail.source} />}
      {detail.kind === 'hosted' && <HostedToolDetail part={detail.part} source={detail.source} />}
    </FloatingPanel>
  );
}
