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
	// shellSweepEvery spaces the age sweeps of one results folder.
	shellSweepEvery = 10 * time.Minute
	shellLabelBytes = 60
)

// shellAsyncSpec names what differs between the tools whose child process can outlive the
// call as a handle: tool_smith and exec.
type shellAsyncSpec struct {
	kind      asyncmgr.Kind
	idPrefix  string
	idPattern *regexp.Regexp
	// noun starts the "still running" line.
	noun        string
	labelPrefix string
	// resultsDir is the folder of finished results inside the async folder.
	resultsDir string
}

// shellRun is one child that runs as a handle.
type shellRun struct {
	watch  *outputWatch
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

func (r *shellRun) stopWith(reason string) {
	r.mu.Lock()
	if r.reason == "" {
		r.reason = reason
	}
	r.mu.Unlock()
	r.cancel()
}

func (r *shellRun) stopReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reason
}

// shellResultPath is where the full result of run id is kept.
func (h *AsyncHost) shellResultPath(spec shellAsyncSpec, id string) string {
	return filepath.Join(h.dir, spec.resultsDir, id+".txt")
}

// attachShell registers the driver of the spec's kind once.
func (h *AsyncHost) attachShell(spec shellAsyncSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shellDrivers[spec.kind] {
		return
	}
	if h.shellDrivers == nil {
		h.shellDrivers = map[asyncmgr.Kind]bool{}
	}
	h.shellDrivers[spec.kind] = true
	_ = h.manager.AddDriver(&shellDriver{host: h, spec: spec})
}

func (h *AsyncHost) shellRun(id string) *shellRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shellRuns[id]
}

func (h *AsyncHost) setShellRun(run *shellRun) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shellRuns == nil {
		h.shellRuns = map[string]*shellRun{}
	}
	h.shellRuns[run.id] = run
}

func (h *AsyncHost) dropShellRun(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.shellRuns, id)
}

// sweepShellResults removes result files older than the longest time the manager keeps a
// handle, at most once per shellSweepEvery for each folder.
func (h *AsyncHost) sweepShellResults(spec shellAsyncSpec) {
	cfg := h.Config()
	days := max(cfg.RetentionDays, cfg.UnreadTTLDays)
	if days <= 0 {
		return
	}
	h.mu.Lock()
	if time.Since(h.shellSwept[spec.resultsDir]) < shellSweepEvery {
		h.mu.Unlock()
		return
	}
	if h.shellSwept == nil {
		h.shellSwept = map[string]time.Time{}
	}
	h.shellSwept[spec.resultsDir] = time.Now()
	h.mu.Unlock()
	dir := filepath.Join(h.dir, spec.resultsDir)
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

// startShellAsync runs the child as a handle of the async manager and blocks up to blockMs
// for it. run gets a context of its own: only a stop, the child's timeout or the end of the
// server ends it, never the end of the call.
func startShellAsync(ctx context.Context, host *AsyncHost, spec shellAsyncSpec, execution tools.ExecutionContext, label string, blockMs *int64, run func(context.Context) shellRunResult) (string, error) {
	callID := execution.CallID
	owner, _ := handleOwner(execution)
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The run id could not be created",
			tools.Information{Key: "system_error", Value: err.Error()}), nil
	}
	id := spec.idPrefix + hex.EncodeToString(random)
	if err := host.Manager().Register(asyncmgr.Registration{ID: id, Owner: owner, Kind: spec.kind, Label: label}); err != nil {
		code := "spawn_failed"
		if errors.Is(err, asyncmgr.ErrCapacity) {
			code = "async_capacity_reached"
		}
		return tools.ExecutionError(callID, tools.StatusError, code, err.Error()), nil
	}
	host.sweepShellResults(spec)

	runCtx, cancel := context.WithCancel(context.Background())
	watch := newOutputWatch()
	runCtx = withOutputWatch(runCtx, watch)
	handle := &shellRun{id: id, cancel: cancel, done: make(chan struct{}), start: time.Now(), label: label, watch: watch}
	host.setShellRun(handle)
	go func() {
		defer close(handle.done)
		defer cancel()
		result := run(runCtx)
		handle.text = result.text
		ref := host.shellResultPath(spec, id)
		if err := os.MkdirAll(filepath.Dir(ref), 0o700); err != nil || os.WriteFile(ref, []byte(result.text), 0o600) != nil {
			ref = ""
		}
		outcome := shellAsyncResult(result, handle.stopReason(), int(host.Config().SummaryMaxBytes))
		outcome.Ref = ref
		host.dropShellRun(id)
		// The signal is queued before done closes, so a call released by done finds it.
		_, _ = host.Manager().Complete(id, outcome)
	}()

	timer := time.NewTimer(host.Config().BlockDuration(blockMs))
	defer timer.Stop()
	select {
	case <-handle.done:
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-handle.done:
		host.Manager().MarkRead(owner, id)
		return handle.text, nil
	default:
	}
	text, err := tools.RenderToolResult(tools.ResultOptions{
		ID:      callID,
		Status:  tools.StatusOK,
		Summary: fmt.Sprintf("%s %s is still running. Do not start it again: use the async tool with id %s to wait for the result (action wait), look at it (status) or stop it", spec.noun, id, id),
		Information: append([]tools.Information{
			{Key: "run", Value: id},
			{Key: "state", Value: "running"},
		}, host.latestInformation(owner, id)...),
	})
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "tool_output_failed", "The result could not be rendered"), nil
	}
	return text, nil
}

// shellLabel is the first line of the program, cut for the handle list.
func shellLabel(spec shellAsyncSpec, body string) string {
	label, cut := cutUTF8(oneLine(body), shellLabelBytes)
	if cut {
		label += "..."
	}
	return spec.labelPrefix + label
}

// shellAsyncResult maps a finished child to the unified result: a stop is stopped, a
// timeout is timed_out, a failure before or while starting is failed, any exit is done (ok
// when the code is 0 and no signal ended it). The summary is the program's answer, else the
// tail of its output.
func shellAsyncResult(run shellRunResult, stopReason string, summaryMax int) asyncmgr.Result {
	child := run.child
	if child == nil || child.spawnError != nil {
		return asyncmgr.Result{State: asyncmgr.StateFailed, Reason: "the program could not be started", Summary: oneLine(run.text), Cut: false}
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

// shellDriver is the async driver of one spec's kind: it stops a running child. The run
// reports its own end.
type shellDriver struct {
	host *AsyncHost
	spec shellAsyncSpec
}

func (d *shellDriver) Kind() asyncmgr.Kind { return d.spec.kind }

func (d *shellDriver) Stop(id, reason string) {
	if run := d.host.shellRun(id); run != nil {
		run.stopWith(reason)
	}
}

func (d *shellDriver) latest(id string, info latestInfo, prev any, cfg asyncmgr.Config, now time.Time) (string, any) {
	run := d.host.shellRun(id)
	if run == nil {
		return "", prev
	}
	return outputLatest(run.watch.get(), info, prev, cfg, now)
}

func (d *shellDriver) Detail(id string, maxBytes int) string {
	if !d.spec.idPattern.MatchString(id) {
		return ""
	}
	if run := d.host.shellRun(id); run != nil {
		return fmt.Sprintf("%s is running: %ds used", run.label, int64(time.Since(run.start).Seconds()))
	}
	data, err := os.ReadFile(d.host.shellResultPath(d.spec, id))
	if err != nil {
		return ""
	}
	text, _ := cutUTF8(strings.TrimSpace(string(data)), int64(maxBytes))
	return text
}
