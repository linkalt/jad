package native

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"jad/backend-go/internal/asyncmgr"
)

const asyncKindSubagent asyncmgr.Kind = "subagent"

// subagentAsyncFilesBytes bounds the changed-file line at the head of a handle's summary.
const subagentAsyncFilesBytes = 200

// subagentAsyncHandle reports one read-only sub-agent run to the async manager as a handle
// owned by the session that started it.
type subagentAsyncHandle struct {
	host  *AsyncHost
	owner string
	label string
}

func newSubagentAsyncHandle(host *AsyncHost, owner, agent, task string) *subagentAsyncHandle {
	return &subagentAsyncHandle{host: host, owner: owner, label: agent + ": " + task}
}

func (h *subagentAsyncHandle) begin(id string, queued bool) error {
	return h.host.Manager().Register(asyncmgr.Registration{ID: id, Owner: h.owner, Kind: asyncKindSubagent, Label: h.label, Queued: queued})
}

func (h *subagentAsyncHandle) markRunning(id string) { h.host.Manager().MarkRunning(id) }

// abort ends a handle whose run never started; nobody is told about it.
func (h *subagentAsyncHandle) abort(id string, cause error) {
	manager := h.host.Manager()
	_, _ = manager.Complete(id, asyncmgr.Result{State: asyncmgr.StateFailed, Reason: cause.Error()})
	manager.MarkRead(h.owner, id)
}

// end reports the final state of the run. It is called before the run's done channel
// closes, so a caller released by done finds the signal queued.
//
// answer is the child's whole answer, before the cut to result_max_bytes that the run's result
// carries: it is saved in the result file behind ref, and bytes is its size, so the cut a
// summary shows is never the end of the answer. transcript is the child's messages.jsonl.
func (h *subagentAsyncHandle) end(run subagentRunView, transcript, answer string) {
	result := subagentAsyncResult(run)
	if full := subagentFinalText(run, answer); full != "" {
		result.Ref = h.host.saveResult(string(asyncKindSubagent), run.id, full)
		result.Bytes = int64(len(full))
	}
	result.RefTranscript = transcript
	_, _ = h.host.Manager().Complete(run.id, result)
}

// markRead consumes the signal of a run whose end the call just reported.
func (h *subagentAsyncHandle) markRead(id string) { h.host.Manager().MarkRead(h.owner, id) }

// subagentAsyncResult maps a run's final state to the unified one. ok means the run is
// done; every other state keeps its own name with ok false. The summary and the detail are
// the answer, or the partial text a cut-off child left, or the failure reason.
func subagentAsyncResult(run subagentRunView) asyncmgr.Result {
	result := subagentAsyncOutcome(run)
	// The files a writer changed come first, so the summary's cut keeps them. The answer
	// gives up what the files line takes, so the summary stays within the run's limit.
	// The worktree line is not cut: it holds the branch name the parent needs whole.
	if files := subagentFilesText(run); files != "" {
		answer, cut := cutUTF8(result.Summary, max(run.resultMax-int64(len(files))-1, 0))
		result.Summary = strings.TrimSpace(files + "\n" + answer)
		result.Cut = result.Cut || cut
	}
	// The record keeps the whole text, as profile_message and a remote run do, so wait and
	// status deliver the answer (to the response budget) instead of its summary_max_bytes head.
	result.Detail = result.Summary
	return result
}

// subagentFilesText is the lines that come before the answer: the changed files (cut), then the
// worktree and sandbox lines (whole).
func subagentFilesText(run subagentRunView) string {
	files, _ := cutUTF8(changedListText(run), subagentAsyncFilesBytes)
	return strings.TrimSpace(files + "\n" + strings.TrimSpace(worktreeText(run)+"\n"+sandboxText(run)))
}

// subagentFinalText is what the result file of a run holds: the lines before the answer, then
// the whole answer. Empty when the child left no text.
func subagentFinalText(run subagentRunView, answer string) string {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return ""
	}
	return strings.TrimSpace(subagentFilesText(run) + "\n" + answer)
}

func subagentAsyncOutcome(run subagentRunView) asyncmgr.Result {
	text := strings.TrimSpace(run.result.Text)
	result := asyncmgr.Result{Reason: run.reason, Summary: text, Bytes: int64(len(text)), Cut: run.cut, ResumedFrom: run.resumedFrom, ConfigNote: run.configNote}
	switch run.state {
	case subagentRunDone:
		result.State, result.OK = asyncmgr.StateDone, true
	case subagentRunStopped:
		result.State = asyncmgr.StateStopped
	case subagentRunTimedOut:
		result.State = asyncmgr.StateTimedOut
	case subagentRunLost:
		result.State = asyncmgr.StateLost
	default:
		result.State = asyncmgr.StateFailed
		result.Summary = boundedReason(run.reason)
	}
	return result
}

// subagentTranscriptRef is where the full work of a run is: its transcript, once the child has
// one (a run that ended before it started has none). The model reads it with the normal read tool.
func subagentTranscriptRef(run *subagentRun) string {
	run.mu.Lock()
	dir := run.dir
	run.mu.Unlock()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "messages.jsonl")
}

// subagentDriver is the async driver of kind subagent: it finds the sub-agent manager that
// owns a run and asks it to stop. The run reports its own end.
type subagentDriver struct{ host *AsyncHost }

func (d *subagentDriver) Kind() asyncmgr.Kind { return asyncKindSubagent }

func (d *subagentDriver) Stop(id, reason string) {
	if run := d.find(id); run != nil {
		run.stopWith(reason)
	}
}

func (d *subagentDriver) latest(id string, info latestInfo, prev any, cfg asyncmgr.Config, now time.Time) (string, any) {
	run := d.find(id)
	if run == nil {
		return "", prev
	}
	run.mu.Lock()
	dir := run.dir
	run.mu.Unlock()
	return activityLatest(transcriptOf(dir), info, prev, cfg, now)
}

func (d *subagentDriver) Detail(id string, maxBytes int) string {
	run := d.find(id)
	if run == nil {
		return ""
	}
	view := run.view()
	if view.state == subagentRunQueued || view.state == subagentRunRunning {
		text := fmt.Sprintf("agent %s is %s: %ds of %d ms used\n%s", view.agent, view.state, int64(view.elapsed.Seconds()), view.timeoutMS, view.task)
		if files := changedFilesText(view); files != "" {
			text += "\n" + files
		}
		return text
	}
	run.loadText()
	view = run.view()
	text := strings.TrimSpace(view.result.Text)
	if files := changedFilesText(view); files != "" {
		text = strings.TrimSpace(files + "\n" + text)
	}
	text, _ = cutUTF8(text, int64(maxBytes))
	return text
}

func (d *subagentDriver) find(id string) *subagentRun {
	for _, manager := range d.host.subagentManagers() {
		if run := manager.byID(id); run != nil {
			return run
		}
	}
	return nil
}
