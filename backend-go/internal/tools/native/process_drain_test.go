//go:build !windows

package native

import (
	"path/filepath"
	"testing"
	"time"
)

// tools.native.process.drain_after_exit_ms is how long Wait keeps reading output a
// descendant still holds open after the shell exited; it used to be fixed at 500 ms.
func TestProcessUsesTheConfiguredDrainAfterExit(t *testing.T) {
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	defer manager.Close()
	request := processTestRequest(directory, "sleep 30 & echo started", 1024)
	const drain = 1500 * time.Millisecond
	request.Policy.DrainAfterExitMs = int64(drain / time.Millisecond)
	started := time.Now()
	snapshot, err := manager.Start(request)
	if err != nil {
		t.Fatal(err)
	}
	result := manager.Wait(snapshot.ID, 10_000)
	took := time.Since(started)
	if !result.Waited {
		t.Fatalf("the process did not end within 10 s: %+v", result.Snapshot)
	}
	if took < drain-200*time.Millisecond {
		t.Fatalf("the process ended after %v; with a held pipe it must wait the configured %v", took, drain)
	}
}

func TestProcessDrainAfterExitDefaultsToTheBashValue(t *testing.T) {
	if got := processDrainAfterExit(0); got != bashDrainAfterExit {
		t.Fatalf("drain for an unset policy = %v, want %v", got, bashDrainAfterExit)
	}
	if got := processDrainAfterExit(70); got != 70*time.Millisecond {
		t.Fatalf("drain for 70 ms = %v", got)
	}
}
