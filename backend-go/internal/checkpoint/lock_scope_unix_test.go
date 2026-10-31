//go:build unix

package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// lockScopeFixture gives the first service a git wrapper that logs each call and
// blocks its write-tree on a FIFO. The second service uses a wrapper that only logs,
// and both share one staging directory, so they share one gate and one state lock.
type lockScopeFixture struct {
	f       *fixture
	second  *Service
	entered string
	release string
	logPath string
}

func newLockScopeFixture(t *testing.T) *lockScopeFixture {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	out := &lockScopeFixture{
		entered: filepath.Join(dir, "entered"),
		release: filepath.Join(dir, "release"),
		logPath: filepath.Join(dir, "git.log"),
	}
	armed := filepath.Join(dir, "armed")
	for _, fifo := range []string{out.entered, out.release} {
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := func(name, gate string) string {
		path := filepath.Join(dir, name)
		script := "#!/bin/sh\n" +
			"for arg in \"$@\"; do\n" +
			"  if [ \"$arg\" = write-tree ] && [ -e '" + armed + "' ] && [ '" + gate + "' = yes ]; then\n" +
			"    echo in > '" + out.entered + "'; read _ < '" + out.release + "'\n" +
			"  fi\n" +
			"done\n" +
			"for arg in \"$@\"; do last=$arg; done\n" +
			"echo '" + name + " '\"$last\" >> '" + out.logPath + "'\n" +
			"exec '" + realGit + "' \"$@\"\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	firstBinary := wrapper("first", "yes")
	secondBinary := wrapper("second", "no")
	out.f = newFixture(t, func(settings *sessionfs.CheckpointSettings) { settings.GitBinary = firstBinary })
	secondSettings := out.f.service.options.Settings
	secondSettings.GitBinary = secondBinary
	out.second = NewService(Options{
		Settings: secondSettings, Workspace: out.f.workspace, StagingDir: out.f.staging,
		HarnessPaths: out.f.service.options.HarnessPaths, Durability: "process",
	})
	out.f.seed(t)
	out.f.begin(t, "turn-1", 1)
	out.f.write(t, "a.txt", "a1\n")
	if err := os.WriteFile(armed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

// startFinish runs the first service's Finish in the background and waits, bounded,
// until its write-tree is blocked on the FIFO.
func (l *lockScopeFixture) startFinish(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	id := ""
	for _, record := range l.f.service.List() {
		if record.State == StateOpen {
			id = record.ID
		}
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		l.f.service.Finish(id)
	}()
	entered := make(chan struct{})
	go func() {
		if _, err := os.ReadFile(l.entered); err == nil {
			close(entered)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Finish never reached write-tree")
	}
	unblock := func() {
		if err := os.WriteFile(l.release, []byte("go\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	return finished, unblock
}

// gitLog returns the logged git calls in order, each as "<service> <subcommand>".
func (l *lockScopeFixture) gitLog(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(l.logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// Assertion 1: while the first Finish is blocked inside git, the second service's
// Begin runs no git call until that Finish's git phase has returned.
func TestSecondServiceRunsNoGitWhileFirstFinishIsInsideGit(t *testing.T) {
	l := newLockScopeFixture(t)
	finished, unblock := l.startFinish(t)
	begun := make(chan struct{})
	go func() {
		defer close(begun)
		l.second.Begin(context.Background(), "turn-2", 2)
	}()
	select {
	case <-begun:
		unblock()
		<-finished
		t.Fatal("the second Begin returned while the first Finish was still blocked in git")
	case <-time.After(2 * time.Second):
	}
	if seen := l.gitLog(t); slices.ContainsFunc(seen, func(line string) bool { return strings.HasPrefix(line, "second ") }) {
		unblock()
		<-finished
		t.Fatalf("the second service ran git while the first Finish was blocked: %v", seen)
	}
	unblock()
	<-finished
	<-begun
}

// Assertion 2: once both operations have finished, no git call from the second
// service sits between two git calls of the first service.
func TestGitCallsOfTwoOperationsDoNotInterleave(t *testing.T) {
	l := newLockScopeFixture(t)
	finished, unblock := l.startFinish(t)
	begun := make(chan struct{})
	go func() {
		defer close(begun)
		l.second.Begin(context.Background(), "turn-2", 2)
	}()
	unblock()
	<-finished
	<-begun
	seen := l.gitLog(t)
	first, last := -1, -1
	for index, line := range seen {
		if strings.HasPrefix(line, "first ") {
			if first < 0 {
				first = index
			}
			last = index
		}
	}
	if first < 0 {
		t.Fatalf("the first service ran no git: %v", seen)
	}
	for index := first; index <= last; index++ {
		if strings.HasPrefix(seen[index], "second ") {
			t.Fatalf("a second-service git call sits inside the first service's git calls: %v", seen)
		}
	}
}

// Assertion 3: the state lock (s.mu) is not held while the first Finish's git call is
// in flight. At HEAD Finish holds s.mu across capture and keep, so TryLock fails.
func TestStateLockIsFreeWhileFinishIsInsideGit(t *testing.T) {
	l := newLockScopeFixture(t)
	finished, unblock := l.startFinish(t)
	defer func() {
		unblock()
		<-finished
	}()
	if !l.f.service.mu.TryLock() {
		t.Fatal("the state lock is held while the first Finish's write-tree is blocked in git")
	}
	l.f.service.mu.Unlock()
}

// Assertion 4: a Restore on the second service runs no git call while the first
// Finish is blocked inside git. Restore holds the gate for its whole git phase, so
// its first git call waits for the Finish to release it.
func TestRestoreRunsNoGitWhileFinishIsInsideGit(t *testing.T) {
	l := newLockScopeFixture(t)
	finished, unblock := l.startFinish(t)
	id := l.f.service.List()[0].ID
	restored := make(chan struct{})
	go func() {
		defer close(restored)
		_, _ = l.second.Restore(context.Background(), id, RestoreOptions{Mode: ModeFull, DryRun: true})
	}()
	select {
	case <-restored:
		unblock()
		<-finished
		t.Fatal("the Restore returned while the first Finish was still blocked in git")
	case <-time.After(2 * time.Second):
	}
	if seen := l.gitLog(t); slices.ContainsFunc(seen, func(line string) bool { return strings.HasPrefix(line, "second ") }) {
		unblock()
		<-finished
		t.Fatalf("the Restore ran git while the first Finish was blocked: %v", seen)
	}
	unblock()
	<-finished
	<-restored
}

// Assertion 5: a gc a prune asked for runs only once the gate is free. While the
// first Finish is blocked inside git, the second service's reclaim runs no git call,
// and it runs after the Finish releases the gate.
func TestReclaimWaitsForTheGateWhileFinishIsInsideGit(t *testing.T) {
	l := newLockScopeFixture(t)
	finished, unblock := l.startFinish(t)
	l.second.mu.Lock()
	l.second.gcPending = true
	l.second.mu.Unlock()
	l.second.startPendingReclaim()
	reclaimed := make(chan struct{})
	go func() {
		defer close(reclaimed)
		l.second.reclaiming.Wait()
	}()
	select {
	case <-reclaimed:
		unblock()
		<-finished
		t.Fatal("the reclaim finished while the first Finish was still blocked in git")
	case <-time.After(2 * time.Second):
	}
	if seen := l.gitLog(t); slices.ContainsFunc(seen, func(line string) bool { return strings.HasPrefix(line, "second ") }) {
		unblock()
		<-finished
		t.Fatalf("the reclaim ran git while the first Finish was blocked: %v", seen)
	}
	unblock()
	<-finished
	<-reclaimed
	if seen := l.gitLog(t); !slices.ContainsFunc(seen, func(line string) bool { return strings.HasPrefix(line, "second ") }) {
		t.Fatalf("the reclaim never ran git after the first Finish released the gate: %v", seen)
	}
}
