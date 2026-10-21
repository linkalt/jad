package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"jad/backend-go/internal/httpapi"
)

// newCatalogBenchmarkResolver is a catalog-layout root with sessionCount sessions of
// turnsPerSession completed turns each.
func newCatalogBenchmarkResolver(b *testing.B, sessionCount, turnsPerSession int) *Resolver {
	b.Helper()
	return newManySessionsResolver(b, sessionCount, turnsPerSession)
}

// BenchmarkListSessions measures the session list a polling frontend issues:
// "conditional" repeats the request with the current ETag (the 304 path) and
// "unconditional" rebuilds the whole body.
func BenchmarkListSessions(b *testing.B) {
	for _, sessionCount := range []int{4, 20, 200} {
		resolver := newCatalogBenchmarkResolver(b, sessionCount, 50)
		ctx := context.Background()
		first, err := resolver.ListSessions(ctx, "", nil)
		if err != nil {
			b.Fatal(err)
		}
		if listed := strings.Count(string(first.Body), `"session_name"`); listed != sessionCount {
			b.Fatalf("list holds %d sessions, want %d", listed, sessionCount)
		}
		b.Run(fmt.Sprintf("conditional/%d-sessions", sessionCount), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				response, err := resolver.ListSessions(ctx, first.ETag, nil)
				if err != nil || !response.NotModified {
					b.Fatalf("conditional list: notModified=%v err=%v", response.NotModified, err)
				}
			}
		})
		b.Run(fmt.Sprintf("unconditional/%d-sessions", sessionCount), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				response, err := resolver.ListSessions(ctx, "", nil)
				if err != nil || len(response.Body) == 0 {
					b.Fatalf("unconditional list: err=%v", err)
				}
			}
		})
	}
}

func BenchmarkReadSessionSnapshotConditional(b *testing.B) {
	resolver := newCatalogBenchmarkResolver(b, 1, 50)
	ctx := context.Background()
	first, err := resolver.ReadSessionSnapshot(ctx, "session-0000", "", httpapi.SnapshotOptions{}, "", nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		response, err := resolver.ReadSessionSnapshot(ctx, "session-0000", "", httpapi.SnapshotOptions{}, first.ETag, nil)
		if err != nil || !response.NotModified {
			b.Fatalf("conditional snapshot: notModified=%v err=%v", response.NotModified, err)
		}
	}
}
