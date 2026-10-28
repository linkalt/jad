package httpapi

import (
	"testing"
	"time"
)

func TestRegistryCloseWaitsForACancelledRunToFinish(t *testing.T) {
	registry := newRunRegistry(time.Minute, 4096, 0)
	run, err := registry.begin("alpha\x00", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		<-run.context().Done()
		time.Sleep(100 * time.Millisecond)
		close(finished)
		run.finish("stopped", nil)
	}()
	registry.close(10 * time.Second)
	select {
	case <-finished:
	default:
		t.Fatal("close returned before the cancelled run finished")
	}
}

func TestRegistryCloseGivesUpOnARunIgnoringCancellation(t *testing.T) {
	registry := newRunRegistry(time.Minute, 4096, 0)
	run, err := registry.begin("alpha\x00", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	defer run.finish("stopped", nil)
	started := time.Now()
	registry.close(100 * time.Millisecond)
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("close took %v, want about the 100ms timeout", elapsed)
	}
}
