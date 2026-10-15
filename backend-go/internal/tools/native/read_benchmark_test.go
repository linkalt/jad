package native

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// BenchmarkNativeReadLargeFile reads 50 lines from a text file of the given size.
func BenchmarkNativeReadLargeFile(b *testing.B) {
	for _, megabytes := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("%dMB", megabytes), func(b *testing.B) {
			workspace := b.TempDir()
			line := strings.Repeat("a line of source text ", 3) + "\n"
			content := strings.Repeat(line, (megabytes<<20)/len(line))
			if err := os.WriteFile(filepath.Join(workspace, "big.txt"), []byte(content), 0o600); err != nil {
				b.Fatal(err)
			}
			tool, err := NewReadTool(sessionfs.NativeReadSettings{
				Policy:       sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
				MaxBytes:     64 << 10,
				MaxFileBytes: 128 << 20,
				DefaultLimit: 250,
			})
			if err != nil {
				b.Fatal(err)
			}
			catalog, err := tools.NewCatalog([]tools.Tool{tool})
			if err != nil {
				b.Fatal(err)
			}
			call := []byte(`{"type":"function_call","call_id":"c","name":"read","arguments":"{\"path\":\"big.txt\",\"offset\":100,\"limit\":50}"}`)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				output, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: workspace})
				if err != nil || !strings.Contains(string(output), "total_line_count") {
					b.Fatalf("read failed: %v %s", err, output)
				}
			}
		})
	}
}
