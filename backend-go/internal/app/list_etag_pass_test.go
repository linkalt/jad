package app

import (
	"context"
	"testing"
)

// The first list of a session with an unwritten usage cache scans the transcript and
// writes the cache. The ETag it returns must be the one the next poll computes, and a
// poll with nothing changed must stay a 304.
func TestListETagSurvivesTheScanThatWritesTheUsageCache(t *testing.T) {
	resolver := newManySessionsResolver(t, 3, 4)
	ctx := context.Background()
	first, err := resolver.ListSessions(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		next, err := resolver.ListSessions(ctx, first.ETag, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !next.NotModified {
			t.Fatalf("round %d: the ETag of the scanning list is not the ETag of the next poll", round)
		}
	}
}
