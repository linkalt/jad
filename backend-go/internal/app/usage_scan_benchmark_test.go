package app

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// BenchmarkScanTranscriptIndexRebuild measures the usage pass over a whole transcript:
// the usage cache is removed before each scan, so every record is decoded.
func BenchmarkScanTranscriptIndexRebuild(b *testing.B) {
	for _, turns := range []int{200, 2000} {
		b.Run(fmt.Sprintf("%d-turns", turns), func(b *testing.B) {
			resolver := newProfileTurnsBenchmarkResolver(b, 1, turns)
			location, err := sessionfs.Resolve(resolver.roots, "demo")
			if err != nil {
				b.Fatal(err)
			}
			config, err := sessionfs.LoadSessionSettings(location, "dev/p00", "")
			if err != nil {
				b.Fatal(err)
			}
			shared, err := resolver.sharedStore(config)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = os.Remove(config.UsageCachePath)
				index, err := scanTranscriptIndex(shared, config)
				if err != nil {
					b.Fatal(err)
				}
				if index.usage.CompletedResponses != int64(turns) {
					b.Fatalf("completed responses = %d, want %d", index.usage.CompletedResponses, turns)
				}
			}
		})
	}
}

func TestScanTranscriptIndexRejectsCompletedRecordWithoutObjectResponse(t *testing.T) {
	resolver := newProfileTurnsBenchmarkResolver(t, 1, 1)
	location, err := sessionfs.Resolve(resolver.roots, "demo")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "dev/p00", "")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(config.MessagesPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(`{"type":"response.completed","session_turn":1,"response":"oops"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_, err = scanTranscriptIndex(shared, config)
	if err == nil || !strings.Contains(err.Error(), "must contain an object response") {
		t.Fatalf("scan error = %v, want the object-response error", err)
	}
}
