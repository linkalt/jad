package imagestore

import (
	"encoding/base64"
	"encoding/binary"
	"sync/atomic"
	"testing"
)

func uniquePNG(size int, serial uint64) string {
	data := make([]byte, size)
	copy(data, "\x89PNG\r\n\x1a\n")
	binary.BigEndian.PutUint64(data[8:], serial)
	return base64.StdEncoding.EncodeToString(data)
}

// BenchmarkPutPayloadParallel stores distinct images from many goroutines. Store.mu is
// held over the Stat and the temp-file write, so ns/op should fall with -cpu only if the
// write is not serialised. "encode-only" is the payload construction alone.
func BenchmarkPutPayloadParallel(b *testing.B) {
	const imageBytes = 16 << 10
	b.Run("encode-only", func(b *testing.B) {
		var serial atomic.Uint64
		b.SetBytes(imageBytes)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = uniquePNG(imageBytes, serial.Add(1))
			}
		})
	})
	b.Run("store-16KB", func(b *testing.B) {
		store := New(Options{Dir: b.TempDir(), MinBytes: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 40})
		var serial atomic.Uint64
		b.SetBytes(imageBytes)
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, ok := store.putPayload("image/png", uniquePNG(imageBytes, serial.Add(1))); !ok {
					b.Error("putPayload refused the image")
					return
				}
			}
		})
	})
}
