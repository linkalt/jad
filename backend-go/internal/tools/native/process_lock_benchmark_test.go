package native

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newBenchProcessManager(b *testing.B, running int) *ManagedProcessManager {
	b.Helper()
	if runtime.GOOS == "windows" {
		b.Skip("managed process groups require a POSIX platform")
	}
	manager := NewProcessManager(filepath.Join(b.TempDir(), "processes"))
	for index := 0; index < running; index++ {
		id := fmt.Sprintf("proc_bench%04d", index)
		manager.manifests[id] = ProcessManifest{SchemaVersion: 1, ID: id, State: ProcessExited, StartedAt: "2026-01-01T00:00:00.000Z"}
	}
	return manager
}

// BenchmarkPersistManifestParallel is N goroutines persisting the manifest of N different
// processes. persistMu is manager-wide and held over a file create, write, fsync and
// rename, so the fsyncs of unrelated processes queue behind each other: ns/op per call
// does not fall with -cpu if that serialises.
func BenchmarkPersistManifestParallel(b *testing.B) {
	manager := newBenchProcessManager(b, 0)
	var serial atomic.Int64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		id := fmt.Sprintf("proc_p%04d", serial.Add(1))
		manifest := ProcessManifest{SchemaVersion: 1, ID: id, State: ProcessRunning, StartedAt: "2026-01-01T00:00:00.000Z"}
		for pb.Next() {
			if err := manager.persistManifest(manifest); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkManagerLockWaitDuringStartSection holds the same steps Start runs under
// manager.mu (the capacity scan and os.MkdirAll of the process directory) in a loop while
// a poller measures how long manager.mu takes to acquire and release.
func BenchmarkManagerLockWaitDuringStartSection(b *testing.B) {
	for _, shape := range []struct {
		name    string
		holders bool
	}{{"idle", false}, {"mkdir-under-lock", true}} {
		b.Run(shape.name, func(b *testing.B) {
			manager := newBenchProcessManager(b, 100)
			var stop atomic.Bool
			var group sync.WaitGroup
			if shape.holders {
				group.Add(1)
				go func() {
					defer group.Done()
					for serial := 0; !stop.Load(); serial++ {
						manager.mu.Lock()
						running := 0
						for _, manifest := range manager.manifests {
							if manifest.State == ProcessRunning {
								running++
							}
						}
						_ = running
						_ = os.MkdirAll(filepath.Join(manager.directory, fmt.Sprintf("d%d", serial)), 0o755)
						manager.mu.Unlock()
					}
				}()
			}
			waits := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				started := time.Now()
				manager.mu.Lock()
				manager.mu.Unlock()
				waits = append(waits, time.Since(started))
			}
			b.StopTimer()
			stop.Store(true)
			group.Wait()
			sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
			b.ReportMetric(float64(waits[len(waits)/2].Nanoseconds()), "p50-ns")
			b.ReportMetric(float64(waits[len(waits)*99/100].Nanoseconds()), "p99-ns")
			b.ReportMetric(float64(waits[len(waits)-1].Microseconds()), "max-us")
		})
	}
}

// BenchmarkProcessListWhileStarting is the status poll (List: manager.mu, then a snapshot
// of every entry) while another goroutine starts and finishes short processes back to
// back, which is more often than any session does. "idle" is the poll alone.
func BenchmarkProcessListWhileStarting(b *testing.B) {
	for _, shape := range []struct {
		name     string
		starting bool
	}{{"idle", false}, {"starting", true}} {
		b.Run(shape.name, func(b *testing.B) {
			if runtime.GOOS == "windows" {
				b.Skip("managed process groups require a POSIX platform")
			}
			workspace := b.TempDir()
			manager := NewProcessManager(filepath.Join(workspace, "processes"))
			b.Cleanup(func() { _ = manager.Close() })
			var stop atomic.Bool
			var starts atomic.Int64
			var group sync.WaitGroup
			if shape.starting {
				request := processTestRequest(workspace, "true", 1<<20)
				request.Policy.MaxRunning = 1000
				group.Add(1)
				go func() {
					defer group.Done()
					for !stop.Load() {
						if _, err := manager.Start(request); err != nil {
							return
						}
						starts.Add(1)
					}
				}()
			}
			waits := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				started := time.Now()
				_ = manager.List("running")
				waits = append(waits, time.Since(started))
				if shape.starting {
					time.Sleep(50 * time.Microsecond)
				}
			}
			b.StopTimer()
			stop.Store(true)
			group.Wait()
			sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
			b.ReportMetric(float64(waits[len(waits)/2].Nanoseconds()), "p50-ns")
			b.ReportMetric(float64(waits[len(waits)*99/100].Nanoseconds()), "p99-ns")
			b.ReportMetric(float64(waits[len(waits)-1].Microseconds()), "max-us")
			b.ReportMetric(float64(starts.Load()), "starts")
		})
	}
}
