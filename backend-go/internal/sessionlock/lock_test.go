package sessionlock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	helperLockPathEnv = "JAD_SESSIONLOCK_HELPER_PATH"
	helperStartedEnv  = "JAD_SESSIONLOCK_HELPER_STARTED"
	helperAcquiredEnv = "JAD_SESSIONLOCK_HELPER_ACQUIRED"
)

func TestAcquireSerializesAcrossProcesses(t *testing.T) {
	if lockPath := os.Getenv(helperLockPathEnv); lockPath != "" {
		runLockHelper(t, lockPath)
		return
	}

	lockPath := filepath.Join(t.TempDir(), ".session.lock")
	parentLock, err := Acquire(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("acquire parent lock: %v", err)
	}
	defer parentLock.Close()

	startedPath := lockPath + ".started"
	acquiredPath := lockPath + ".acquired"
	cmd := exec.Command(os.Args[0], "-test.run=^TestAcquireSerializesAcrossProcesses$")
	cmd.Env = append(os.Environ(),
		helperLockPathEnv+"="+lockPath,
		helperStartedEnv+"="+startedPath,
		helperAcquiredEnv+"="+acquiredPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock helper: %v", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	waitForFile(t, startedPath)
	if waitForFileFor(acquiredPath, 100*time.Millisecond) {
		t.Fatal("child acquired the lock while parent still held it")
	}

	if err := parentLock.Close(); err != nil {
		t.Fatalf("release parent lock: %v", err)
	}
	waitForFile(t, acquiredPath)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock helper failed: %v", err)
	}
	finished = true
}

func TestAcquireHonorsCancellationWhileWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".session.lock")
	held, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatalf("acquire initial lock: %v", err)
	}
	defer held.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire while held error = %v, want context deadline", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	lock, err := Acquire(context.Background(), filepath.Join(t.TempDir(), ".session.lock"))
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func runLockHelper(t *testing.T, lockPath string) {
	t.Helper()
	startedPath := os.Getenv(helperStartedEnv)
	acquiredPath := os.Getenv(helperAcquiredEnv)
	if startedPath == "" || acquiredPath == "" {
		t.Fatal("lock helper paths are missing")
	}
	if err := os.WriteFile(startedPath, []byte("started"), 0o600); err != nil {
		t.Fatalf("write started marker: %v", err)
	}
	lock, err := Acquire(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("acquire child lock: %v", err)
	}
	if err := os.WriteFile(acquiredPath, []byte("acquired"), 0o600); err != nil {
		t.Fatalf("write acquired marker: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("release child lock: %v", err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat marker %q: %v", path, err)
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for marker %q", path)
		case <-ticker.C:
		}
	}
}

func waitForFileFor(path string, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		} else if !errors.Is(err, os.ErrNotExist) {
			return false
		}
		select {
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}
