package app

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkRunStartParallelSessions is LockSession plus Resolve for 16 different sessions
// from parallel goroutines (one run start each): Resolver.mu and the shared store map are
// the only state they share. ns/op falls with -cpu if the starts do not serialise.
func BenchmarkRunStartParallelSessions(b *testing.B) {
	const sessions = 16
	ctx := context.Background()
	resolver := newManySessionsResolver(b, sessions, 20)
	old := time.Now().Add(-time.Hour)
	layout := resolver.roots[0].Layout
	for _, dir := range []string{resolver.roots[0].Directory, layout.Profiles, layout.Models, layout.Tools, layout.MCP} {
		_ = filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
			if err == nil {
				_ = os.Chtimes(path, old, old)
			}
			return nil
		})
	}
	var next atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		reference := fmt.Sprintf("session-%04d", next.Add(1)%sessions)
		for pb.Next() {
			release, err := resolver.LockSession(ctx, reference, "", "")
			if err != nil {
				b.Error(err)
				return
			}
			if _, err := resolver.Resolve(ctx, reference, "", ""); err != nil {
				b.Error(err)
				release()
				return
			}
			release()
		}
	})
}
