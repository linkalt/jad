package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
)

type noopDriver struct{}

func (noopDriver) Kind() asyncmgr.Kind { return "process" }
func (noopDriver) Stop(string, string) {}
func (noopDriver) Forget(string)       {}

func asyncStateContextConfig(maxBytes int64) sessionfs.SessionConfig {
	return sessionfs.SessionConfig{
		StateContext: sessionfs.StateContextSettings{
			Enabled: true, Header: sessionfs.DefaultStateContextHeader, Role: "developer",
			Triggers: compactionTrigger([]string{"async"}, maxBytes),
		},
		NativeAsync: &sessionfs.NativeAsyncSettings{Config: asyncmgr.DefaultConfig()},
	}
}

func openAsyncManager(t *testing.T, mutate func(*asyncmgr.Config)) *asyncmgr.Manager {
	t.Helper()
	cfg := asyncmgr.DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	manager, err := asyncmgr.Open(asyncmgr.Options{Dir: filepath.Join(t.TempDir(), ".async"), Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AddDriver(noopDriver{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager
}

func TestAsyncStateContextNoticeIsBoundedAndHoldsPendingHandlesOnly(t *testing.T) {
	manager := openAsyncManager(t, func(c *asyncmgr.Config) { c.ListDefaultLimit = 4; c.MaxHandles = 40 })
	for i := 0; i < 9; i++ {
		if err := manager.Register(asyncmgr.Registration{ID: fmt.Sprintf("live%d", i), Owner: "me", Kind: "process", Label: "job"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.Register(asyncmgr.Registration{ID: "gone", Owner: "me", Kind: "process"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Complete("gone", asyncmgr.Result{State: asyncmgr.StateDone, OK: true, Summary: "SECRET-SUMMARY"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Wait(context.Background(), "me", []string{"gone"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(asyncmgr.Registration{ID: "theirs", Owner: "you", Kind: "process"}); err != nil {
		t.Fatal(err)
	}

	snapshot, include, err := collectStateContextSnapshot(asyncStateContextConfig(4096), stateContextSources{async: manager, owner: "me"}, "compaction")
	if err != nil || !include {
		t.Fatalf("include = %v, err = %v", include, err)
	}
	if got := strings.Count(snapshot.Text, `"kind": "process"`); got != 4 {
		t.Fatalf("listed %d handles, want list_default_limit 4:\n%s", got, snapshot.Text)
	}
	for _, banned := range []string{"gone", "theirs", "SECRET-SUMMARY"} {
		if strings.Contains(snapshot.Text, banned) {
			t.Fatalf("the notice contains %q:\n%s", banned, snapshot.Text)
		}
	}
	if !strings.Contains(snapshot.Text, `"active": 9`) || !strings.Contains(snapshot.Text, `"has_more": true`) {
		t.Fatalf("counts or has_more missing:\n%s", snapshot.Text)
	}
}

func TestAsyncStateContextNoticeShrinksToMaxBytesAndStaysEmptyWithoutHandles(t *testing.T) {
	manager := openAsyncManager(t, func(c *asyncmgr.Config) { c.ListDefaultLimit = 50; c.MaxHandles = 40 })
	for i := 0; i < 30; i++ {
		if err := manager.Register(asyncmgr.Registration{ID: fmt.Sprintf("handle-number-%02d", i), Owner: "me", Kind: "process", Label: "a label"}); err != nil {
			t.Fatal(err)
		}
	}
	config := asyncStateContextConfig(1200)
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{async: manager, owner: "me"}, "compaction")
	if err != nil || !include {
		t.Fatalf("include = %v, err = %v", include, err)
	}
	if int64(len(snapshot.Text)) > 1200 {
		t.Fatalf("notice is %d bytes, max_bytes is 1200", len(snapshot.Text))
	}
	if !strings.Contains(snapshot.Text, `"omitted_by_snapshot"`) {
		t.Fatalf("a trimmed notice must say so:\n%s", snapshot.Text)
	}

	empty, include, err := collectStateContextSnapshot(asyncStateContextConfig(4096), stateContextSources{async: openAsyncManager(t, nil), owner: "me"}, "compaction")
	if err != nil || include {
		t.Fatalf("a session without handles wrote a block: include = %v err = %v text %q", include, err, empty.Text)
	}
}

func TestAsyncStateContextWithoutAnAsyncManagerFailsLoudly(t *testing.T) {
	_, _, err := collectStateContextSnapshot(asyncStateContextConfig(4096), stateContextSources{owner: "me"}, "compaction")
	if err == nil || !strings.Contains(err.Error(), "async") {
		t.Fatalf("error = %v, want the missing async manager", err)
	}
}
