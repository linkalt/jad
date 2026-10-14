import type {ReactNode} from 'react';
import {List, ListItem, Text, VStack} from '@astryxdesign/core';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';
import appConfig from '../config.json';
import {FloatingPanel} from './FloatingPanel';

type RunSettingsPanelProps = {
  region: ResizableRegion;
  onClose: () => void;
  sessionTitle: string;
  sessionTurns: number;
  isLiveSession: boolean;
};

/** Values are shown in the code face, so numbers and ids stay unambiguous. */
function Value({children}: {children: ReactNode}) {
  return (
    <Text type="code" color="secondary">
      {children}
    </Text>
  );
}

const onOff = (value: boolean) => (value ? 'on' : 'off');

/** What the `@` menu falls back to when the session listing is unavailable. */
const mentionFallback =
  appConfig.mention_files.length > 0
    ? `${appConfig.mention_files.length} configured`
    : 'built-in list';

/** Where the `@` menu's paths come from, and what stands in when that fails. */
const mentionSource =
  appConfig.mention_source === 'cwd' ? `session files, else ${mentionFallback}` : mentionFallback;

/**
 * Second dataset for the shared left panel: the configuration the running app
 * actually uses, read straight from `config.json` plus the active session.
 *
 * It is a read-only view on purpose — the values are edited in the file, and
 * showing them here answers "which flags are on right now?" without a reload.
 */
export function RunSettingsPanel({
  region,
  onClose,
  sessionTitle,
  sessionTurns,
  isLiveSession,
}: RunSettingsPanelProps) {
  return (
    <FloatingPanel
      side="start"
      label="Run settings"
      title="Run settings"
      subtitle="Read-only view of the running configuration"
      region={region}
      onClose={onClose}
    >
      <VStack gap={5}>
        <List header={<Text type="label">Session</Text>} hasDividers>
          <ListItem label="Active session" endContent={<Value>{sessionTitle}</Value>} />
          <ListItem label="Turns" endContent={<Value>{sessionTurns}</Value>} />
          <ListItem
            label="Transport"
            endContent={<Value>{isLiveSession ? 'live stream' : 'mock'}</Value>}
          />
          <ListItem
            label="Refresh interval"
            endContent={<Value>{appConfig.session_refresh_interval_ms}ms</Value>}
          />
          <ListItem
            label="Mock run delay"
            endContent={<Value>{appConfig.mock_run_delay_ms}ms</Value>}
          />
        </List>

        <List header={<Text type="label">Transcript</Text>} hasDividers>
          <ListItem
            label="Mounted window"
            endContent={<Value>{appConfig.transcript_window_messages} messages</Value>}
          />
          <ListItem
            label="Lane width range"
            endContent={
              <Value>
                {appConfig.conversation_lane_min_size}–{appConfig.conversation_lane_max_size}px
              </Value>
            }
          />
          <ListItem
            label="Tool steps visible per provider run"
            endContent={
              <Value>
                {appConfig.message_step_window} ({appConfig.message_step_window_page} per reveal)
              </Value>
            }
          />
          <ListItem
            label="Reveal page"
            endContent={<Value>{appConfig.transcript_window_page_messages} messages</Value>}
          />
          <ListItem
            label="Defer off-screen rows"
            endContent={<Value>{onOff(appConfig.defer_offscreen_transcript_rows)}</Value>}
          />
          <ListItem
            label="Row height estimate"
            endContent={<Value>{appConfig.transcript_row_estimate_px}px</Value>}
          />
          <ListItem
            label="Reveal settle"
            endContent={<Value>{appConfig.scroll_settle_frames} frames</Value>}
          />
          <ListItem
            label="Scroll probe"
            endContent={<Value>{onOff(appConfig.scroll_debug)}</Value>}
          />
          <ListItem
            label="Tool output cap"
            endContent={<Value>{appConfig.tool_output_max_chars} chars</Value>}
          />
        </List>

        <List header={<Text type="label">Composer</Text>} hasDividers>
          <ListItem
            label="Markdown shortcuts"
            endContent={<Value>{onOff(appConfig.composer_markdown_shortcuts)}</Value>}
          />
          <ListItem
            label="Markdown preview"
            endContent={<Value>{onOff(appConfig.composer_preview_enabled)}</Value>}
          />
          <ListItem
            label="Paste as token"
            endContent={
              <Value>
                {appConfig.paste_as_token_enabled
                  ? `over ${appConfig.paste_as_token_threshold} chars`
                  : 'off'}
              </Value>
            }
          />
          <ListItem
            label="File mentions (@)"
            endContent={
              <Value>
                {appConfig.mention_files_enabled
                  ? `${mentionSource}, max ${appConfig.mention_max_results}`
                  : 'off'}
              </Value>
            }
          />
          <ListItem
            label="Attach context"
            endContent={<Value>{onOff(appConfig.attach_context_enabled)}</Value>}
          />
          <ListItem
            label="Preserve line breaks"
            endContent={<Value>{onOff(appConfig.preserve_line_breaks)}</Value>}
          />
        </List>

        <List header={<Text type="label">Panels</Text>} hasDividers>
          <ListItem
            label="Inspector on start"
            endContent={<Value>{onOff(appConfig.show_inspector_by_default)}</Value>}
          />
          <ListItem
            label="README on start"
            endContent={<Value>{onOff(appConfig.show_readme_by_default)}</Value>}
          />
          <ListItem
            label="Left panel on start"
            endContent={<Value>{onOff(appConfig.show_prompt_library_by_default)}</Value>}
          />
          <ListItem
            label="Outside click closes"
            endContent={<Value>{onOff(appConfig.close_panels_on_outside_click)}</Value>}
          />
        </List>
      </VStack>
    </FloatingPanel>
  );
}
