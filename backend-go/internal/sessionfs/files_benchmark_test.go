package sessionfs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkCollectSessionFiles1000Entries(b *testing.B) {
	root := b.TempDir()
	for index := range 1000 {
		name := filepath.Join(root, fmt.Sprintf("file-%04d.md", index))
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	settings := defaultSessionFileListingSettings()
	settings.MaxDepth = 1
	settings.MaxEntries = 1200
	settings.MaxResults = 1200
	b.ResetTimer()
	for range b.N {
		listing := CollectSessionFiles(root, settings, NativePathPolicy{})
		if len(listing.Files) != 1000 || listing.Truncated {
			b.Fatalf("listing files=%d truncated=%t", len(listing.Files), listing.Truncated)
		}
	}
}
