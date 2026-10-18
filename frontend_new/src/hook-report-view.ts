/**
 * @file hook-report-view.ts
 * @input The `hook_report` run-stream event: one run_start, run_end or pre_compact command's report
 * @output A validated report and the activity notification that words it
 * @position Pure model behind the "Hook ..." entries in the Notifications panel
 *
 * The server writes these reports to its log and sends them live on the run stream.
 * They are never stored, so a reload does not bring them back.
 */

import type {NewActivityNotification} from './activity-notifications.ts';

export interface HookReport {
  event: string;
  trigger: string;
  report: string;
}

const HOOK_EVENTS = ['run_start', 'run_end', 'pre_compact'];

export function parseHookReport(value: unknown): {turnId: string; report: HookReport} | undefined {
  if (typeof value !== 'object' || value === null) return undefined;
  const {turn_id: turnId, event, trigger, report} = value as Record<string, unknown>;
  if (typeof turnId !== 'string' || turnId === '') return undefined;
  if (typeof event !== 'string' || !HOOK_EVENTS.includes(event)) return undefined;
  if (typeof report !== 'string' || report === '') return undefined;
  return {turnId, report: {event, trigger: typeof trigger === 'string' ? trigger : '', report}};
}

/** Every report is information; a failed or timed out command is worded as an error. */
export function hookReportNotification(
  sessionId: string,
  sessionTitle: string,
  report: HookReport,
): NewActivityNotification {
  const failed = /^\[[^\]\n]+\] (failed|timed out|could not start|stopped before)/.test(report.report);
  const when = report.trigger === '' ? report.event : `${report.event} (${report.trigger})`;
  return {
    sessionId,
    sessionTitle,
    kind: failed ? 'error' : 'info',
    title: failed ? `Hook failed: ${when}` : `Hook ran: ${when}`,
    message: report.report,
  };
}
