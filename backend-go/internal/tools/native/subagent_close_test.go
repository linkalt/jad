package native

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Close does not wait for a child that ignores its cancellation for ever: it waits the
// close grace, logs the runs still going and returns, so shutdown is bounded.
func TestSubagentManagerCloseReturnsAfterTheGraceWhenAChildIgnoresCancellation(t *testing.T) {
	logged := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	manager := NewSubagentManager()
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		<-stuck
		return SubagentResult{Text: "late", Rounds: 1}, nil
	}}
	run, err := manager.start(subagentStart{
		launcher: launcher, owner: "o",
		request:   SubagentRequest{Agent: agentNamed("explore"), Task: "t"},
		admission: runAdmission{session: "s", limit: 1, maxQueued: 1}, keep: 4, resultMax: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(began); waited > closeGrace+time.Second {
		t.Fatalf("Close waited %v for a child that ignores cancellation, want about %v", waited, closeGrace)
	}
	if !strings.Contains(logged.String(), run.id) {
		t.Fatalf("Close did not log the run still going:\n%s", logged.String())
	}
}
