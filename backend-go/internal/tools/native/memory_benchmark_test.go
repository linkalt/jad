package native

import (
	"path/filepath"
	"testing"
)

func BenchmarkMemoryStoreList100Records(b *testing.B) {
	root := filepath.Join(b.TempDir(), "memory")
	policy := memoryTestSettings(root)
	policy.MaxRecordsPerType = 100
	policy.MaxTotalRecords = 100
	policy.MaxRecordBytes = 4 * 1024
	policy.MaxTotalBytes = 1 << 20
	policy.ListLimit = 100
	policy.MaxListLimit = 100
	policy.MaxListBytes = 1 << 20
	policy.MaxScanRecords = 200
	policy.MaxScanBytes = 1 << 20
	store := NewMemoryStore(root)
	for index := 0; index < 100; index++ {
		if _, err := store.Create("bench", map[string]any{"index": index, "value": "payload"}, policy); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.List("bench", nil, 100, "newest", nil, policy); err != nil {
			b.Fatal(err)
		}
	}
}
