package native

import (
	"context"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// A call that is cancelled while its command runs must not leave a goroutine behind: the
// readers and the waiter all end once the process group is signalled.
func TestRunBashCommandCancelLeavesNoGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	child := runBashCommand(ctx, bashChildRequest{
		file:        "/bin/sh",
		args:        []string{"-c", "sleep 30"},
		cwd:         t.TempDir(),
		environment: []string{"PATH=/usr/bin:/bin"},
	})
	if child.spawnError != nil {
		t.Skip("cannot start /bin/sh: " + child.spawnError.Error())
	}

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutines after a cancelled call = %d, want at most %d", got, baseline)
	}
}

// readersDone must stay open until the second reader exits, and must close exactly once. A
// second close would panic, so the test would fail loudly if the count were wrong.
func TestReadBashPipeClosesReadersDoneOnceWhenBothExit(t *testing.T) {
	collector := newBashOutputCollector(bashStreamLimits{}, bashStreamLimits{}, 0, true)
	request := bashChildRequest{collector: collector}
	var readersLeft atomic.Int32
	readersLeft.Store(2)
	readersDone := make(chan struct{})

	stdoutReader, stdoutWriter := mustBashPipe(t)
	stderrReader, stderrWriter := mustBashPipe(t)
	defer stdoutReader.Close()
	defer stderrReader.Close()

	stdoutExited := make(chan struct{})
	go func() {
		readBashPipe(stdoutReader, bashStdout, request, &readersLeft, readersDone)
		close(stdoutExited)
	}()
	_ = stdoutWriter.Close()
	<-stdoutExited
	assertBashReadersOpen(t, readersDone, "after the first reader exited")

	stderrExited := make(chan struct{})
	go func() {
		readBashPipe(stderrReader, bashStderr, request, &readersLeft, readersDone)
		close(stderrExited)
	}()
	_ = stderrWriter.Close()
	<-stderrExited
	select {
	case <-readersDone:
	case <-time.After(5 * time.Second):
		t.Fatal("readersDone is still open after both readers exited")
	}
}

func mustBashPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	return reader, writer
}

func assertBashReadersOpen(t *testing.T, readersDone chan struct{}, when string) {
	t.Helper()
	select {
	case <-readersDone:
		t.Fatalf("readersDone closed %s, want open", when)
	default:
	}
}
