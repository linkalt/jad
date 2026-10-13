package httpapi

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/serverconfig"
)

func BenchmarkRunContentCacheAppendAndSnapshot(b *testing.B) {
	for _, mode := range []struct {
		name        string
		memoryLimit int
	}{
		{name: "memory", memoryLimit: 1 << 20},
		{name: "spill", memoryLimit: 1024},
	} {
		b.Run(mode.name, func(b *testing.B) {
			const chunkBytes = 8 * 1024
			content := strings.Repeat("x", 1<<20)
			partialPath := filepath.Join(b.TempDir(), "responses.partial")
			b.SetBytes(int64(len(content)))
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				cache := newRunContentCache(mode.memoryLimit, serverconfig.DefaultRunContentWriteBatchBytes, time.Duration(serverconfig.DefaultRunContentFlushIntervalMS)*time.Millisecond)
				cache.SetPartialPath(partialPath)
				for offset := 0; offset < len(content); offset += chunkBytes {
					end := offset + chunkBytes
					if end > len(content) {
						end = len(content)
					}
					cache.Append(content[offset:end])
				}
				if got := cache.Snapshot(); len(got) != len(content) {
					b.Fatalf("snapshot length = %d, want %d", len(got), len(content))
				}
				cache.Close(true)
			}
		})
	}
}
