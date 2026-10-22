package native

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"jad/backend-go/internal/asyncmgr"
)

const (
	asyncKindProcess asyncmgr.Kind = "process"
	// processLabelMaxBytes keeps the command out of the unified record beyond a short label.
	processLabelMaxBytes = 80
)

// processAsyncHandle reports one started process to the async manager as a handle owned
// by the session that started it.
type processAsyncHandle struct {
	host       *AsyncHost
	owner      string
	label      string
	storageDir string
}

func newProcessAsyncHandle(host *AsyncHost, owner, command, storageDir string) *processAsyncHandle {
	label := strings.Join(strings.Fields(command), " ")
	if len(label) > processLabelMaxBytes {
		cut := processLabelMaxBytes
		for cut > 0 && !utf8.RuneStart(label[cut]) {
			cut--
		}
		label = label[:cut]
	}
	return &processAsyncHandle{host: host, owner: owner, label: label, storageDir: storageDir}
}

func (h *processAsyncHandle) Begin(id string) error {
	return h.host.Manager().Register(asyncmgr.Registration{ID: id, Owner: h.owner, Kind: asyncKindProcess, Label: h.label})
}

func (h *processAsyncHandle) End(snapshot ProcessSnapshot) {
	result := processAsyncResult(snapshot, int(h.host.Config().SummaryMaxBytes))
	result.Ref = filepath.Join(h.storageDir, snapshot.ID)
	_, _ = h.host.Manager().Complete(snapshot.ID, result)
}

func (h *processAsyncHandle) Abort(id string, cause error) {
	manager := h.host.Manager()
	_, _ = manager.Complete(id, asyncmgr.Result{State: asyncmgr.StateFailed, Reason: cause.Error()})
	manager.MarkRead(h.owner, id)
}

// processAsyncResult maps a finished process to the unified result: an exit is done (ok
// when the code is 0), a stop is stopped, a start or wait failure is failed. The summary
// is the tail of stderr for a failure, else of stdout, cut to summaryMax.
func processAsyncResult(snapshot ProcessSnapshot, summaryMax int) asyncmgr.Result {
	result := asyncmgr.Result{Reason: snapshot.StopReason, Bytes: snapshot.StdoutStoredBytes + snapshot.StderrStoredBytes}
	zero := false
	if value, exists := processManifestValue(snapshot.ExitCode); exists && value != nil {
		result.Code = fmt.Sprint(value)
		zero = result.Code == "0"
	} else if value, exists := processManifestValue(snapshot.Signal); exists && value != nil {
		result.Code = fmt.Sprint(value)
	}
	switch snapshot.State {
	case ProcessExited:
		result.State, result.OK = asyncmgr.StateDone, zero
	case ProcessStopped:
		result.State = asyncmgr.StateStopped
	case ProcessLost:
		result.State = asyncmgr.StateLost
	default:
		result.State = asyncmgr.StateFailed
	}
	text := snapshot.Output.stdout.text
	if !zero || snapshot.State != ProcessExited {
		if snapshot.Output.stderr.text != "" {
			text = snapshot.Output.stderr.text
		}
	}
	result.Summary, result.Cut = tailUTF8(text, summaryMax)
	return result
}

// tailUTF8 keeps the last limit bytes of text, starting on a character boundary.
func tailUTF8(text string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if len(text) <= limit {
		return text, false
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:], true
}

// processDriver is the async driver of kind process: it finds the process manager that
// owns a handle and asks it to stop. The manager reports the end itself.
type processDriver struct{ host *AsyncHost }

func (d *processDriver) Kind() asyncmgr.Kind { return asyncKindProcess }

func (d *processDriver) Stop(id, reason string) {
	for _, stopper := range d.host.processManagers() {
		if stopper.hasProcess(id) {
			stopper.stopAsync(id, reason)
			return
		}
	}
}

func (d *processDriver) Detail(id string, maxBytes int) string {
	for _, stopper := range d.host.processManagers() {
		if stopper.hasProcess(id) {
			return stopper.detailText(id, maxBytes)
		}
	}
	return ""
}

// processStopper is what the process driver needs from a process manager.
type processStopper interface {
	hasProcess(id string) bool
	stopAsync(id, reason string)
	detailText(id string, maxBytes int) string
}

func (manager *ManagedProcessManager) hasProcess(id string) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.entries[id] != nil
}

// stopAsync asks the process to stop without making the caller wait for the kill grace;
// the manager's own waiter reports the end.
func (manager *ManagedProcessManager) stopAsync(id, reason string) {
	go manager.Stop(id, reason)
}

// detailText is the live output of a running process, cut to its last maxBytes.
func (manager *ManagedProcessManager) detailText(id string, maxBytes int) string {
	snapshot, found := manager.Snapshot(id)
	if !found {
		return ""
	}
	text, _ := tailUTF8(snapshot.Output.text, maxBytes)
	return text
}
