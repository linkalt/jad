import assert from 'node:assert/strict';
import test from 'node:test';

import {hookReportNotification, parseHookReport} from './hook-report-view.ts';

test('a valid hook_report is read and anything malformed is dropped', () => {
  const good = {turn_id: 't1', event: 'run_end', trigger: '', report: '[audit] passed (3 ms, in .)'};
  assert.deepEqual(parseHookReport(good), {turnId: 't1', report: {event: 'run_end', trigger: '', report: '[audit] passed (3 ms, in .)'}});
  for (const bad of [null, 'x', {}, {...good, turn_id: ''}, {...good, turn_id: 5}, {...good, event: 'pre_tool'}, {...good, event: 3}, {...good, report: ''}, {...good, report: 7}]) {
    assert.equal(parseHookReport(bad), undefined, JSON.stringify(bad));
  }
  assert.equal(parseHookReport({...good, trigger: 4}).report.trigger, '');
});

test('a passing report is information and names the trigger of a compaction', () => {
  const notice = hookReportNotification('s', 'Session', {event: 'pre_compact', trigger: 'manual', report: '[snap] passed (5 ms, in .)'});
  assert.deepEqual(notice, {sessionId: 's', sessionTitle: 'Session', kind: 'info', title: 'Hook ran: pre_compact (manual)', message: '[snap] passed (5 ms, in .)'});
  assert.equal(hookReportNotification('s', 'S', {event: 'run_start', trigger: '', report: '[a] passed (1 ms, in .)'}).title, 'Hook ran: run_start');
});

test('a failing, timed out, unstartable or stopped command is an error', () => {
  for (const report of ['[a] failed, exit 2 (4 ms, in .)', '[a] timed out after 1000 ms (setting: hooks.timeout_ms)', '[a] could not start: x (setting: hooks.run_end[a].argv)', '[a] stopped before it finished']) {
    const notice = hookReportNotification('s', 'S', {event: 'run_end', trigger: '', report});
    assert.equal(notice.kind, 'error', report);
    assert.equal(notice.title, 'Hook failed: run_end');
  }
  const passedWithFailureText = hookReportNotification('s', 'S', {event: 'run_end', trigger: '', report: '[a] passed (1 ms, in .)\n[b] failed in the command output'});
  assert.equal(passedWithFailureText.kind, 'info', 'only the status line decides');
});

test('a stop report is read; stop commands skipped at a cap are an error notice', () => {
  const ran = {turn_id: 't1', event: 'stop', trigger: '', report: '[verify] passed (2 ms, in .)'};
  assert.deepEqual(parseHookReport(ran)?.report, {event: 'stop', trigger: '', report: '[verify] passed (2 ms, in .)'});
  assert.equal(hookReportNotification('s', 'S', parseHookReport(ran).report).title, 'Hook ran: stop');
  const skipped = {turn_id: 't1', event: 'stop', trigger: 'cap_reached', report: '[stop hooks] not run: it used 70 tokens, which reached the max_run_tokens limit of 50.'};
  assert.deepEqual(hookReportNotification('s', 'S', parseHookReport(skipped).report), {
    sessionId: 's', sessionTitle: 'S', kind: 'error', title: 'Hook skipped: stop (cap reached)', message: skipped.report,
  });
});
