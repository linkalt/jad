package app

import (
	"context"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// A fork reads the source transcript from a snapshot, so the journal lock is not held while
// the fork writes each line: another reader of the source does not wait for the copy.
func TestForkCopyDoesNotHoldTheSourceJournalLock(t *testing.T) {
	f := newHeldResetFixture(t)
	close(f.release)
	f.runTurn("local:one", "first")
	size := int64(len(f.transcript("one")))
	resolver := f.service.resolver
	location, err := sessionfs.Resolve(resolver.roots, "local:one")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	shared, release, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	lines := 0
	err = resolver.copyForkLines(context.Background(), config, forkPoint{end: size, turns: 1}, func(int64, []byte) error {
		lines++
		if lines > 1 {
			return nil
		}
		read := make(chan error, 1)
		go func() {
			read <- shared.store.ReadRecordsRange(0, size, func(int64, []byte) error { return nil })
		}()
		select {
		case err := <-read:
			return err
		case <-time.After(2 * time.Second):
			t.Error("a reader of the source waited for the fork's copy of a line")
			return nil
		}
	})
	if err != nil || lines < 2 {
		t.Fatalf("copy = %d lines, %v", lines, err)
	}
}
