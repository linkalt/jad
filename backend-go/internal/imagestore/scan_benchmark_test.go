package imagestore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkDirectorySize is the scan a Store makes before its first write: one
// ReadDir plus one Info (lstat) per stored image.
func BenchmarkDirectorySize(b *testing.B) {
	for _, files := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("%d-files", files), func(b *testing.B) {
			dir := b.TempDir()
			for index := 0; index < files; index++ {
				sum := sha256.Sum256([]byte(fmt.Sprint(index)))
				name := hex.EncodeToString(sum[:]) + ".png"
				if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			if got := directorySize(dir); got != int64(files) {
				b.Fatalf("directorySize = %d, want %d", got, files)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				directorySize(dir)
			}
		})
	}
}
