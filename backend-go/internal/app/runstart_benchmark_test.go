package app

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkRunStartLockAndResolve(b *testing.B) {
	ctx := context.Background()
	resolver := newManySessionsResolver(b, 1, 20)
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
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		release, err := resolver.LockSession(ctx, "session-0000", "", "")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := resolver.Resolve(ctx, "session-0000", "", ""); err != nil {
			b.Fatal(err)
		}
		release()
	}
}
