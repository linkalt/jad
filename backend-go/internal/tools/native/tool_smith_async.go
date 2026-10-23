package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

const (
	asyncKindToolSmith asyncmgr.Kind = "tool_smith"
	// toolSmithResultsDir is the folder of finished script results inside the async folder.
	toolSmithResultsDir = "tool_smith"
	// toolSmithSweepEvery spaces the age sweeps of that folder.
	toolSmithSweepEvery = 10 * time.Minute
	toolSmithLabelBytes = 60
)

var toolSmithRunID = regexp.MustCompile(`^smith_[0-9a-f]{8}$`)

// toolSmithRun is one script that runs as a handle.
type toolSmithRun struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}

	// text is the rendered result; it is set before done closes.
	text string

	mu     sync.Mutex
	reason string
	start  time.Time
	label  string
}

func (r *toolSmithRun) stopWith(reason string) {
	r.mu.Lock()
	if r.reason == "" {
		r.reason = reason
	}
	r.mu.Unlock()
	r.cancel()
}

func (r *toolSmithRun) stopReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reason
}

// toolSmithResultPath is where the full result of script id is kept.
func (h *AsyncHost) toolSmithResultPath(id string) string {
	return filepath.Join(h.dir, toolSmithResultsDir, id+".txt")
}

// attachToolSmith registers the driver of kind tool_smith once.
func (h *AsyncHost) attachToolSmith() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.toolSmithSeen {
		return
	}
	h.toolSmithSeen = true
	_ = h.manager.AddDriver(&toolSmithDriver{host: h})
}

func (h *AsyncHost) toolSmithRun(id string) *toolSmithRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.toolSmithRuns[id]
}

func (h *AsyncHost) setToolSmithRun(run *toolSmithRun) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.toolSmithRuns == nil {
		h.toolSmithRuns = map[string]*toolSmithRun{}
	}
	h.toolSmithRuns[run.id] = run
}

func (h *AsyncHost) dropToolSmithRun(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.toolSmithRuns, id)
}

// sweepToolSmithResults removes result files older than the longest time the manager keeps
// a handle, at most once per toolSmithSweepEvery.
func (h *AsyncHost) sweepToolSmithResults() {
	cfg := h.Config()
	days := max(cfg.RetentionDays, cfg.UnreadTTLDays)
	if days <= 0 {
		return
	}
	h.mu.Lock()
	if time.Since(h.toolSmithSwept) < toolSmithSweepEvery {
		h.mu.Unlock()
		return
	}
	h.toolSmithSwept = time.Now()
	h.mu.Unlock()
	dir := filepath.Join(h.dir, toolSmithResultsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// toolSmithNeedsApproval says whether a nested call of the script could ask the user, which
// nobody can answer once the call has returned.
func toolSmithNeedsApproval(script toolSmithScript) bool {
	for _, tool := range script.nested.allowed(script.settings) {
		if tool.Approval == tools.ApprovalAsk {
			return true
		}
	}
	return false
}

// startToolSmithAsync runs the script as a handle of the async manager and blocks up to
// blockMs for it. A script whose nested tools could ask for approval runs to its end inside
// the call instead, where the question has somebody to go to.
func startToolSmithAsync(ctx context.Context, host *AsyncHost, script toolSmithScript, blockMs *int64) (string, error) {
	callID := script.execution.CallID
	owner, _ := handleOwner(script.execution)
	if toolSmithNeedsApproval(script) {
		return runToolSmithScript(ctx, script).text, nil
	}
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The run id could not be created",
			tools.Information{Key: "system_error", Value: err.Error()}), nil
	}
	id := "smith_" + hex.EncodeToString(random)
	label := "script: " + toolSmithLabel(script.body)
	if err := host.Manager().Register(asyncmgr.Registration{ID: id, Owner: owner, Kind: asyncKindToolSmith, Label: label}); err != nil {
		code := "spawn_failed"
		if errors.Is(err, asyncmgr.ErrCapacity) {
			code = "async_capacity_reached"
		}
		return tools.ExecutionError(callID, tools.StatusError, code, err.Error()), nil
	}
	host.sweepToolSmithResults()

	// The script outlives the call: only a stop, the script's own timeout or the end of the
	// server ends it. Nested calls cannot ask anybody once the call has returned.
	runCtx, cancel := context.WithCancel(context.Background())
	run := &toolSmithRun{id: id, cancel: cancel, done: make(chan struct{}), start: time.Now(), label: label}
	host.setToolSmithRun(run)
	script.execution.RequestApproval = backgroundApproval
	script.execution.RequestInteraction = nil
	script.execution.OnInteractionResolved = nil
	go func() {
		defer close(run.done)
		defer cancel()
		result := runToolSmithScript(runCtx, script)
		run.text = result.text
		ref := host.toolSmithResultPath(id)
		if err := os.MkdirAll(filepath.Dir(ref), 0o700); err != nil || os.WriteFile(ref, []byte(result.text), 0o600) != nil {
			ref = ""
		}
		outcome := toolSmithAsyncResult(result, run.stopReason(), int(host.Config().SummaryMaxBytes))
		outcome.Ref = ref
		host.dropToolSmithRun(id)
		// The signal is queued before done closes, so a call released by done finds it.
		_, _ = host.Manager().Complete(id, outcome)
	}()

	timer := time.NewTimer(host.Config().BlockDuration(blockMs))
	defer timer.Stop()
	select {
	case <-run.done:
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-run.done:
		host.Manager().MarkRead(owner, id)
		return run.text, nil
	default:
	}
	text, err := tools.RenderToolResult(tools.ResultOptions{
		ID:      callID,
		Status:  tools.StatusOK,
		Summary: fmt.Sprintf("Script %s is still running. Do not start it again: use the async tool with id %s to wait for the result (action wait), look at it (status) or stop it", id, id),
		Information: []tools.Information{
			{Key: "run", Value: id},
			{Key: "state", Value: "running"},
		},
	})
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "tool_output_failed", "The result could not be rendered"), nil
	}
	return text, nil
}

// toolSmithLabel is the first line of the script, cut for the handle list.
func toolSmithLabel(body string) string {
	label, cut := cutUTF8(oneLine(body), toolSmithLabelBytes)
	if cut {
		return label + "..."
	}
	return label
}

// toolSmithAsyncResult maps a finished script to the unified result: a stop is stopped, a
// timeout is timed_out, a failure before or while starting is failed, any exit is done (ok
// when the code is 0 and no signal ended it). The summary is the script's answer, else the
// tail of its output.
func toolSmithAsyncResult(run shellRunResult, stopReason string, summaryMax int) asyncmgr.Result {
	child := run.child
	if child == nil || child.spawnError != nil {
		return asyncmgr.Result{State: asyncmgr.StateFailed, Reason: "the script could not be started", Summary: oneLine(run.text), Cut: false}
	}
	text := child.output.text
	if run.answer != nil {
		text = run.answer.text
	}
	summary, cut := tailUTF8(text, summaryMax)
	result := asyncmgr.Result{Summary: summary, Cut: cut, Bytes: child.output.returnedBytes}
	if child.hasExitCode {
		result.Code = fmt.Sprint(child.exitCode)
	} else if child.signal != "" {
		result.Code = child.signal
	}
	switch {
	case child.timedOut:
		result.State = asyncmgr.StateTimedOut
	case child.aborted:
		result.State, result.Reason = asyncmgr.StateStopped, stopReason
	default:
		result.State = asyncmgr.StateDone
		result.OK = child.hasExitCode && child.exitCode == 0 && child.signal == ""
	}
	return result
}

// toolSmithDriver is the async driver of kind tool_smith: it stops a running script. The
// run reports its own end.
type toolSmithDriver struct{ host *AsyncHost }

func (d *toolSmithDriver) Kind() asyncmgr.Kind { return asyncKindToolSmith }

func (d *toolSmithDriver) Stop(id, reason string) {
	if run := d.host.toolSmithRun(id); run != nil {
		run.stopWith(reason)
	}
}

func (d *toolSmithDriver) Detail(id string, maxBytes int) string {
	if !toolSmithRunID.MatchString(id) {
		return ""
	}
	if run := d.host.toolSmithRun(id); run != nil {
		return fmt.Sprintf("%s is running: %ds used", run.label, int64(time.Since(run.start).Seconds()))
	}
	data, err := os.ReadFile(d.host.toolSmithResultPath(id))
	if err != nil {
		return ""
	}
	text, _ := cutUTF8(strings.TrimSpace(string(data)), int64(maxBytes))
	return text
}
