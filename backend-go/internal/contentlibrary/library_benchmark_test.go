package contentlibrary

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkLibraryList1000MarkdownFiles(b *testing.B) {
	root := b.TempDir()
	directory := filepath.Join(root, "prompts")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "setting.json"), []byte(`{"libraries":{"prompt_dirs":["prompts"]}}`), 0o600); err != nil {
		b.Fatal(err)
	}
	for index := range 1000 {
		path := filepath.Join(directory, fmt.Sprintf("prompt-%04d.md", index))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("# Prompt %04d\n\nContent for prompt %04d.", index, index)), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	library := New(Options{WorkingDir: root, Environment: map[string]string{}})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := library.List(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
