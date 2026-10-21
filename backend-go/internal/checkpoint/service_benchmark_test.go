package checkpoint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// BenchmarkBeginFinish is one run's checkpoint cost (Begin plus Finish, one file
// changed in between) on a workspace of N small files. It needs git.
func BenchmarkBeginFinish(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git is not installed")
	}
	for _, files := range []int{1000, 20000} {
		b.Run(fmt.Sprintf("%d-files", files), func(b *testing.B) {
			root := b.TempDir()
			workspace, staging := filepath.Join(root, "ws"), filepath.Join(root, "staging")
			if err := os.MkdirAll(staging, 0o755); err != nil {
				b.Fatal(err)
			}
			for index := 0; index < files; index++ {
				dir := filepath.Join(workspace, fmt.Sprintf("d%03d", index%100))
				if index < 100 {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						b.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.txt", index)), []byte(fmt.Sprintf("content %d\n", index)), 0o644); err != nil {
					b.Fatal(err)
				}
			}
			service := NewService(Options{Settings: defaultSettings(), Workspace: workspace, StagingDir: staging, Durability: "process"})
			changed := filepath.Join(workspace, "d000", "f00000.txt")
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				id := service.Begin("turn", int64(iteration))
				if id == "" {
					b.Fatal("Begin took no checkpoint")
				}
				if err := os.WriteFile(changed, []byte(fmt.Sprintf("changed %d\n", iteration)), 0o644); err != nil {
					b.Fatal(err)
				}
				service.Finish(id)
			}
		})
	}
}
