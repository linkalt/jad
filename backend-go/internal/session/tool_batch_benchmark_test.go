package session

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkRunToolBatch is the fan-out cost: N calls over 8 workers, each call a
// no-op (overhead) or a 1 ms sleep (what a real I/O tool looks like).
func BenchmarkRunToolBatch(b *testing.B) {
	for _, work := range []time.Duration{0, time.Millisecond} {
		for _, count := range []int{1, 8, 64} {
			calls := make([]int, count)
			b.Run(fmt.Sprintf("%d-calls/work-%s", count, work), func(b *testing.B) {
				b.ReportAllocs()
				for iteration := 0; iteration < b.N; iteration++ {
					err := runToolBatch(calls, 8,
						func(int) (int, error) {
							if work > 0 {
								time.Sleep(work)
							}
							return 0, nil
						},
						func(int, int) error { return nil },
						nil)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
