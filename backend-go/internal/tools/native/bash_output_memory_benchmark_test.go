package native

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// BenchmarkBashLargeOutputPeak runs a command that prints outMB of text and reports the
// highest heap above the baseline during the call (peak-KiB) and the result size. The
// head and tail budgets are 512 bytes, so the answer is small; the question is whether
// the memory used while collecting depends on the output size. "spill" turns the
// spill-to-file option on.
func BenchmarkBashLargeOutputPeak(b *testing.B) {
	shell, err := exec.LookPath("bash")
	if err != nil {
		b.Skip("bash is not installed")
	}
	for _, spill := range []bool{false, true} {
		for _, outMB := range []int{1, 32, 256} {
			b.Run(fmt.Sprintf("spill-%v/%dMB", spill, outMB), func(b *testing.B) {
				root := b.TempDir()
				settings := sessionfs.NativeBashSettings{
					Policy:           sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
					Shell:            shell,
					TimeoutMs:        60_000,
					MaxTimeoutMs:     60_000,
					KillGraceMs:      50,
					KillProcessGroup: "timeout_only",
					MaxBytes:         2048,
					Stdout:           sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512},
					Stderr:           sessionfs.NativeBashStreamBudget{HeadBytes: 0, TailBytes: 0},
					Spill:            sessionfs.NativeBashSpillSettings{Enabled: false},
					MaxLineChars:     200,
					StripANSI:        true,
					Env:              map[string]string{"NO_COLOR": "1"},
					EnvDeny:          []string{},
				}
				if spill {
					settings.Spill = sessionfs.NativeBashSpillSettings{Enabled: true, CapBytes: 1 << 30, RetentionDays: 1, PruneIntervalSeconds: 3600, StorageDir: b.TempDir()}
				}
				command := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'x' | fold -w 100", outMB<<20)
				execution := tools.ExecutionContext{CallID: "bash-memory", WorkspaceRoot: root, SessionDir: root}
				var peak uint64
				var answer int
				for iteration := 0; iteration < b.N; iteration++ {
					runtime.GC()
					var base runtime.MemStats
					runtime.ReadMemStats(&base)
					done, result := make(chan struct{}), make(chan uint64)
					go func() {
						var high uint64
						for {
							var stats runtime.MemStats
							runtime.ReadMemStats(&stats)
							if stats.HeapAlloc > high {
								high = stats.HeapAlloc
							}
							select {
							case <-done:
								result <- high
								return
							case <-time.After(time.Millisecond):
							}
						}
					}()
					output := executeNativeBash(context.Background(), map[string]any{"command": command}, settings, "", execution)
					close(done)
					if high := <-result; high > base.HeapAlloc && high-base.HeapAlloc > peak {
						peak = high - base.HeapAlloc
					}
					answer = len(output)
					if !strings.Contains(output, "xxxx") {
						b.Fatalf("unexpected result: %.200s", output)
					}
				}
				b.ReportMetric(float64(peak)/1024, "peak-KiB")
				b.ReportMetric(float64(answer), "result-B")
			})
		}
	}
}
