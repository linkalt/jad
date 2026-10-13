package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

func TestResolverSharesAndClosesManagedProcessManager(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	workspace := t.TempDir()
	storageDir := filepath.Join(workspace, "processes")
	resolver, err := NewResolver(
		[]sessionfs.Root{{ID: "local", Directory: workspace}},
		map[string]string{},
		CatalogConfig{MaxPageTurns: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)

	config := sessionfs.SessionConfig{NativeProcess: &sessionfs.NativeProcessSettings{StorageDir: storageDir}}
	manager := resolver.sharedProcessManager(config)
	if manager == nil || manager != resolver.sharedProcessManager(config) {
		t.Fatal("resolver did not reuse the manager for the same storage directory")
	}
	policy := sessionfs.NativeProcessSettings{
		Shell:         "/bin/sh",
		KillGraceMs:   500,
		MaxRunning:    2,
		MaxCompleted:  10,
		RetentionDays: 0,
		Stdout:        sessionfs.NativeBashStreamBudget{HeadBytes: 32, TailBytes: 32},
		Stderr:        sessionfs.NativeBashStreamBudget{HeadBytes: 32, TailBytes: 32},
		LogCapBytes:   128,
		MaxLineChars:  100,
		Env:           map[string]string{},
		EnvDeny:       []string{},
	}
	started, err := manager.Start(native.ProcessStartRequest{
		CWD:         workspace,
		DisplayCWD:  ".",
		Command:     "sleep 30",
		Policy:      policy,
		Environment: os.Environ(),
		APIKeyEnv:   "OPENAI_API_KEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.State != native.ProcessRunning {
		t.Fatalf("initial process state = %q, want running", started.State)
	}

	resolver.Close()
	stopped, found := manager.SnapshotWithLogs(
		started.ID,
		native.ProcessStreamLimits{},
		native.ProcessStreamLimits{},
		0,
		false,
	)
	if !found || stopped == nil {
		t.Fatal("process snapshot missing after resolver close")
	}
	if stopped.State != native.ProcessStopped || stopped.StopReason != "session_closed" {
		t.Fatalf("snapshot after resolver close = state %q, reason %q; want stopped/session_closed", stopped.State, stopped.StopReason)
	}
	resolver.Close()
}
