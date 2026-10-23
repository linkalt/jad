package native

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func backgroundWriterSettings(max int64, agents ...sessionfs.NativeSubagentAgent) sessionfs.NativeSubagentSettings {
	settings := writeSettings(agents...)
	settings.BackgroundWriters = true
	settings.MaxBackgroundWriters = max
	settings.ChangedFilesMax = 100
	return settings
}

func askingWriteTool(name string) tools.Tool {
	tool := writeTool(name)
	tool.Approval = tools.ApprovalAsk
	return tool
}

// backgroundCatalog is buildSubagentCatalog that also hands back the manager and the host.
func backgroundCatalog(t *testing.T, settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher, parent ...tools.Tool) (*tools.Catalog, *SubagentManager, *AsyncHost) {
	t.Helper()
	parentTools := withExecutors(t, parent...)
	host := newTestAsyncHost(t)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := newSubagentToolAsync(settings, parentTools, nil, launcher, nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	return catalogWithAsync(t, host, parentTools, tool), manager, host
}

// writingLauncher records one changed file, then holds the child until release closes or it
// is stopped.
func writingLauncher(release chan struct{}, cancelled *atomic.Int64) *fakeLauncher {
	return &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		if request.Execution.OnFileChange != nil {
			request.Execution.OnFileChange("a.go", "created")
		}
		select {
		case <-release:
			return SubagentResult{Text: "fixed", Rounds: 2, Tokens: 50}, nil
		case <-ctx.Done():
			if cancelled != nil {
				cancelled.Add(1)
			}
			return SubagentResult{}, ctx.Err()
		}
	}}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// callReturnsWithin runs a subagent call and reports its output, or false when it did not
// return in time.
func callReturnsWithin(t *testing.T, catalog *tools.Catalog, arguments string, within time.Duration) (string, bool) {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- subagentCall(t, catalog, arguments) }()
	select {
	case out := <-done:
		return out, true
	case <-time.After(within):
		return "", false
	}
}

func TestBackgroundWriterIsHandedOffAndCollectedThroughAsync(t *testing.T) {
	baseline := baselineCount()
	release := make(chan struct{})
	catalog, manager, host := backgroundCatalog(t, backgroundWriterSettings(1, writerAgent("fixer", "read", "write")), writingLauncher(release, nil), readOnlyTool("read"), writeTool("write"))
	out, returned := callReturnsWithin(t, catalog, `{"task":"fix","block_ms":0}`, 2*time.Second)
	if !returned {
		close(release)
		t.Fatal("a writer with background_writers on and no ask tool did not return a handle for block_ms 0")
	}
	id := runIDOf(t, out)
	for _, want := range []string{"still running", "can change files", "checkpoint"} {
		if !strings.Contains(out, want) {
			t.Errorf("handoff result lacks %q:\n%s", want, out)
		}
	}
	if got := manager.BackgroundWriters(testSessionKey); got != 1 {
		t.Errorf("background writers = %d, want 1", got)
	}
	eventually(t, "the child to record its change", func() bool {
		return strings.Contains(asyncCall(t, catalog, `{"action":"status","id":"`+id+`"}`), "a.go (created)")
	})
	if status := asyncCall(t, catalog, `{"action":"status","id":"`+id+`"}`); !strings.Contains(status, "a.go (created)") {
		t.Errorf("status of the running writer does not list its changed file:\n%s", status)
	}
	close(release)
	got := asyncWait(t, catalog)
	for _, want := range []string{id, "done", "a.go (created)", "fixed"} {
		if !strings.Contains(got, want) {
			t.Errorf("wait lacks %q:\n%s", want, got)
		}
	}
	eventually(t, "the writer place to be given back", func() bool { return manager.BackgroundWriters(testSessionKey) == 0 })
	_ = manager.Close()
	host.Close()
	requireNoGrowth(t, baseline, "a background writer ended")
}

func TestBackgroundWriterOffByDefaultStillRunsToItsEnd(t *testing.T) {
	release := make(chan struct{})
	settings := backgroundWriterSettings(1, writerAgent("fixer", "read", "write"))
	settings.BackgroundWriters = false
	catalog, _, _ := backgroundCatalog(t, settings, writingLauncher(release, nil), readOnlyTool("read"), writeTool("write"))
	defer close(release)
	if out, returned := callReturnsWithin(t, catalog, `{"task":"fix","block_ms":0}`, 200*time.Millisecond); returned {
		t.Fatalf("a writer was handed off without background_writers:\n%s", out)
	}
}

func TestBackgroundWriterWithAnAskSettingToolIsStillHandedOff(t *testing.T) {
	release := make(chan struct{})
	catalog, _, _ := backgroundCatalog(t, backgroundWriterSettings(1, writerAgent("fixer", "read", "write")), writingLauncher(release, nil), readOnlyTool("read"), askingWriteTool("write"))
	defer close(release)
	if out, returned := callReturnsWithin(t, catalog, `{"task":"fix","block_ms":0}`, 2*time.Second); !returned || !strings.Contains(out, "still running") {
		t.Fatalf("a writer was not handed off though no approval gates it:\n%s", out)
	}
}

func TestBackgroundSafeNeedsTheSettingTheAsyncHostAndAWriter(t *testing.T) {
	host := newTestAsyncHost(t)
	plain := subagentAgent{config: writerAgent("w", "read", "write"), nativeTools: withExecutors(t, readOnlyTool("read"), writeTool("write"))}
	asking := subagentAgent{config: plain.config, nativeTools: withExecutors(t, readOnlyTool("read"), askingWriteTool("write"))}
	reader := subagentAgent{config: agentNamed("r"), nativeTools: withExecutors(t, readOnlyTool("read"))}
	on := backgroundWriterSettings(1)
	off := on
	off.BackgroundWriters = false
	for name, tc := range map[string]struct {
		settings sessionfs.NativeSubagentSettings
		agent    subagentAgent
		host     *AsyncHost
		want     bool
	}{
		"all auto":        {on, plain, host, true},
		"setting off":     {off, plain, host, false},
		"no async host":   {on, plain, nil, false},
		"ask-marked tool": {on, asking, host, true},
		"read-only agent": {on, reader, host, false},
	} {
		if got := backgroundSafe(tc.settings, tc.agent, tc.host); got != tc.want {
			t.Errorf("%s: backgroundSafe = %v, want %v", name, got, tc.want)
		}
	}
}

func TestBackgroundWriterOverTheCapRunsToItsEndAndSaysWhy(t *testing.T) {
	release := make(chan struct{})
	catalog, manager, _ := backgroundCatalog(t, backgroundWriterSettings(1, writerAgent("fixer", "read", "write")), writingLauncher(release, nil), readOnlyTool("read"), writeTool("write"))
	first, returned := callReturnsWithin(t, catalog, `{"task":"one","block_ms":0}`, 2*time.Second)
	if !returned {
		close(release)
		t.Fatal("the first writer was not handed off")
	}
	runIDOf(t, first)
	second := make(chan string, 1)
	go func() { second <- subagentCall(t, catalog, `{"task":"two","block_ms":0}`) }()
	select {
	case out := <-second:
		close(release)
		t.Fatalf("a second background writer was handed off over the cap of 1:\n%s", out)
	case <-time.After(200 * time.Millisecond):
	}
	if got := manager.BackgroundWriters(testSessionKey); got != 1 {
		t.Errorf("the capped call took a background place: %d", got)
	}
	close(release)
	select {
	case out := <-second:
		for _, want := range []string{"Agent fixer finished", "Not handed off", "max_background_writers"} {
			if !strings.Contains(out, want) {
				t.Errorf("the capped call's result lacks %q:\n%s", want, out)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the capped call did not return after the children ended")
	}
	eventually(t, "both places to be free", func() bool { return manager.BackgroundWriters(testSessionKey) == 0 })
}

func TestBackgroundWriterCapIsPerSession(t *testing.T) {
	release := make(chan struct{})
	catalog, manager, _ := backgroundCatalog(t, backgroundWriterSettings(1, writerAgent("fixer", "read", "write")), writingLauncher(release, nil), readOnlyTool("read"), writeTool("write"))
	defer close(release)
	for _, session := range []string{"alpha", "beta"} {
		out := subagentCallAs(t, catalog, session, `{"task":"x","block_ms":0}`)
		if !strings.Contains(out, "still running") {
			t.Fatalf("session %s: its first writer was not handed off:\n%s", session, out)
		}
		if got := manager.BackgroundWriters(session); got != 1 {
			t.Errorf("session %s holds %d places, want 1", session, got)
		}
	}
}

func TestBackgroundWriterIsReachedByStopResetAndDelete(t *testing.T) {
	for name, stop := range map[string]func(t *testing.T, catalog *tools.Catalog, host *AsyncHost, id string){
		"async stop": func(t *testing.T, catalog *tools.Catalog, _ *AsyncHost, id string) {
			if out := asyncCall(t, catalog, `{"action":"stop","id":"`+id+`"}`); !strings.Contains(out, "stopped") {
				t.Errorf("async stop:\n%s", out)
			}
		},
		"user stop": func(_ *testing.T, _ *tools.Catalog, host *AsyncHost, _ string) {
			host.Manager().CancelOwner(testSessionKey, "user_stop")
		},
		"reset": func(_ *testing.T, _ *tools.Catalog, host *AsyncHost, _ string) {
			host.Manager().ResetOwner(testSessionKey, "session_reset")
		},
		"delete": func(_ *testing.T, _ *tools.Catalog, host *AsyncHost, _ string) {
			host.Manager().DeleteOwnersWithPrefix(testSessionKey, "session_delete")
		},
	} {
		t.Run(name, func(t *testing.T) {
			baseline := baselineCount()
			var cancelled atomic.Int64
			catalog, manager, host := backgroundCatalog(t, backgroundWriterSettings(1, writerAgent("fixer", "read", "write")), writingLauncher(make(chan struct{}), &cancelled), readOnlyTool("read"), writeTool("write"))
			id := runIDOf(t, subagentCall(t, catalog, `{"task":"fix","block_ms":0}`))
			stop(t, catalog, host, id)
			eventually(t, "the writer to be cancelled", func() bool { return cancelled.Load() == 1 })
			eventually(t, "the writer place to be given back", func() bool { return manager.BackgroundWriters(testSessionKey) == 0 })
			_ = manager.Close()
			host.Close()
			requireNoGrowth(t, baseline, name)
		})
	}
}

func TestBackgroundWriterDoesNotChangeTheListMarkerOfOtherAgents(t *testing.T) {
	agents := []sessionfs.NativeSubagentAgent{agentNamed("reader"), writerAgent("fixer", "read", "write"), writerAgent("careful", "read", "write")}
	list := subagentAgentList(agents, 4096, map[string]bool{"fixer": true})
	for _, want := range []string{"- fixer [can change files; may run in the background]", "- careful [can change files; runs to its end]", "- reader\n"} {
		if !strings.Contains(list+"\n", want) {
			t.Errorf("list lacks %q:\n%s", want, list)
		}
	}
	catalog, _, _ := backgroundCatalog(t, backgroundWriterSettings(1, agents...), &fakeLauncher{}, readOnlyTool("read"), writeTool("write"))
	if out := subagentCall(t, catalog, `{}`); !strings.Contains(out, "fixer [can change files; may run in the background]") {
		t.Errorf("the agent list call does not mark a background writer:\n%s", out)
	}
}

func TestLostBackgroundWriterKeepsItsRecordedFiles(t *testing.T) {
	root := t.TempDir()
	release := make(chan struct{})
	defer close(release)
	first := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = first.Close() })
	host := newTestAsyncHost(t)
	settings := backgroundWriterSettings(1, writerAgent("fixer", "read", "write"))
	settings.StorageDir = root
	parent := withExecutors(t, readOnlyTool("read"), writeTool("write"))
	tool, err := newSubagentToolAsync(settings, parent, nil, writingLauncher(release, nil), nil, first, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"fix","block_ms":0}`))
	eventually(t, "the changed file to reach the manifest", func() bool {
		return len(manifestOf(t, root, id).Files) == 1
	})

	// The server dies here and restarts over the same directory.
	restarted := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = restarted.Close() })
	run := restarted.get(id, testSessionKey)
	if run == nil {
		t.Fatal("the restarted manager lost the run")
	}
	view := run.view()
	if view.state != subagentRunLost || !view.changes || !view.tracked || len(view.files) != 1 || view.files[0].Path != "a.go" {
		t.Fatalf("restored writer = state %s changes %v tracked %v files %v", view.state, view.changes, view.tracked, view.files)
	}
	out := restoredOutput(t, restarted, testSessionKey, id)
	for _, want := range []string{"subagent_lost", "written_files: a.go (created)", "some changes may already be made"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost writer result lacks %q:\n%s", want, out)
		}
	}
	driverText := (&subagentDriver{host: hostWith(t, restarted)}).Detail(id, 4096)
	if !strings.Contains(driverText, "changed files: a.go (created)") {
		t.Errorf("the async status of the lost writer lacks its files:\n%s", driverText)
	}
	if got := restarted.BackgroundWriters(testSessionKey); got != 0 {
		t.Errorf("a lost run holds %d background writer places after the restart", got)
	}
}

// hostWith is a host that knows only manager, so a driver can look runs up in it.
func hostWith(t *testing.T, manager *SubagentManager) *AsyncHost {
	t.Helper()
	host := newTestAsyncHost(t)
	host.attachSubagent(manager)
	return host
}
