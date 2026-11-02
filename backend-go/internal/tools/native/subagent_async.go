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
func (h *subagentAsyncHandle) end(run subagentRunView, ref string) {
	result := subagentAsyncResult(run)
	result.Ref = ref
	_, _ = h.host.Manager().Complete(run.id, result)
}

// markRead consumes the signal of a run whose end the call just reported.
func (h *subagentAsyncHandle) markRead(id string) { h.host.Manager().MarkRead(h.owner, id) }

// subagentAsyncResult maps a run's final state to the unified one. ok means the run is
// done; every other state keeps its own name with ok false. The summary is the answer,
// or the partial text a cut-off child left, or the failure reason.
func subagentAsyncResult(run subagentRunView) asyncmgr.Result {
	result := subagentAsyncOutcome(run)
	// The files a writer changed come first, so the summary's cut keeps them. The answer
	// gives up what the files line takes, so the summary stays within the run's limit.
	if files, _ := cutUTF8(changedFilesText(run), subagentAsyncFilesBytes); files != "" {
		answer, _ := cutUTF8(result.Summary, max(run.resultMax-int64(len(files))-1, 0))
		result.Summary = strings.TrimSpace(files + "\n" + answer)
	}
	return result
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

// subagentRunRef is where the full work of a run is: its transcript once the child has
// one, else the run record. The model reads it with the normal read tool.
func subagentRunRef(run *subagentRun) string {
	run.mu.Lock()
	dir := run.dir
	run.mu.Unlock()
	if dir != "" {
		return filepath.Join(dir, "messages.jsonl")
	}
	if run.store == nil {
		return ""
	}
	return filepath.Join(run.store.dir, run.id+".json")
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
