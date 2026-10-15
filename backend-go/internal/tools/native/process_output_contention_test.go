package native

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func newContentionEntry(t testing.TB) *managedProcess {
	t.Helper()
	dir := t.TempDir()
	return &managedProcess{
		collector: newBashOutputCollector(bashStreamLimits{headBytes: 750, tailBytes: 1200}, bashStreamLimits{headBytes: 750, tailBytes: 1200}, 2000, true),
		stdout:    &bashSpillWriter{path: filepath.Join(dir, "stdout.log"), capBytes: 1 << 30},
		stderr:    &bashSpillWriter{path: filepath.Join(dir, "stderr.log"), capBytes: 1 << 30},
	}
}

// BenchmarkProcessWriteWithStatusPollers is the child's write path (32 KB chunks into a
// real spill file) while pollers call snapshotEntry back to back.
func BenchmarkProcessWriteWithStatusPollers(b *testing.B) {
	chunk := []byte(strings.Repeat("output line of a chatty child process\n", 900))[:32*1024]
	for _, pollers := range []int{0, 1, 4} {
		b.Run(map[int]string{0: "no-poller", 1: "1-poller", 4: "4-pollers"}[pollers], func(b *testing.B) {
			entry := newContentionEntry(b)
			manager := &ManagedProcessManager{}
			writer := processStreamWriter{entry: entry, stream: bashStdout}
			var stop atomic.Bool
			var group sync.WaitGroup
			for index := 0; index < pollers; index++ {
				group.Add(1)
				go func() {
					defer group.Done()
					for !stop.Load() {
						manager.snapshotEntry(entry)
					}
				}()
			}
			b.SetBytes(int64(len(chunk)))
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := writer.Write(chunk); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			stop.Store(true)
			group.Wait()
			entry.stdout.close()
			entry.stderr.close()
			_ = os.Remove(entry.stdout.path)
		})
	}
}

// A snapshot taken while the child is writing must be internally consistent: its
// output never shows more bytes than were produced, and the stored size never passes
// the produced size.
func TestSnapshotEntryIsConsistentWhileWriting(t *testing.T) {
	entry := newContentionEntry(t)
	manager := &ManagedProcessManager{}
	writer := processStreamWriter{entry: entry, stream: bashStdout}
	chunk := []byte(strings.Repeat("x", 100) + "\n")
	var stop atomic.Bool
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		for index := 0; index < 3000; index++ {
			_, _ = writer.Write(chunk)
		}
		stop.Store(true)
	}()
	var last int64
	for !stop.Load() {
		snapshot := manager.snapshotEntry(entry)
		out := snapshot.Output.stdout
		if out.producedBytes < last {
			t.Fatalf("producedBytes went back from %d to %d", last, out.producedBytes)
		}
		last = out.producedBytes
		if out.shownBytes > out.producedBytes {
			t.Fatalf("shown %d > produced %d", out.shownBytes, out.producedBytes)
		}
		if snapshot.StdoutStoredBytes > out.producedBytes+int64(len(chunk)) {
			t.Fatalf("stored %d is far ahead of produced %d", snapshot.StdoutStoredBytes, out.producedBytes)
		}
	}
	group.Wait()
	final := manager.snapshotEntry(entry)
	if want := int64(3000 * len(chunk)); final.Output.stdout.producedBytes != want {
		t.Fatalf("final producedBytes = %d, want %d", final.Output.stdout.producedBytes, want)
	}
	entry.stdout.close()
	entry.stderr.close()
}
