package native

import (
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkRecordsStoreList100Records(b *testing.B) {
	root := filepath.Join(b.TempDir(), "records")
	policy := recordsTestSettings(root)
	policy.MaxRecordsPerType = 100
	policy.MaxTotalRecords = 100
	policy.MaxRecordBytes = 4 * 1024
	policy.MaxTotalBytes = 1 << 20
	policy.ListLimit = 100
	policy.MaxListLimit = 100
	policy.MaxListBytes = 1 << 20
	policy.MaxScanRecords = 200
	policy.MaxScanBytes = 1 << 20
	store := NewRecordsStore(root)
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

// BenchmarkRecordsStoreCreateWithExistingRecords is one create into a store that already
// holds N records of one type (the quota scan stats every record).
func BenchmarkRecordsStoreCreateWithExistingRecords(b *testing.B) {
	for _, existing := range []int{100, 1000, 3000} {
		b.Run(fmt.Sprintf("%d-records", existing), func(b *testing.B) {
			root := filepath.Join(b.TempDir(), "records")
			policy := recordsTestSettings(root)
			policy.MaxRecordsPerType = int64(existing) + int64(b.N) + 10
			policy.MaxTotalRecords = policy.MaxRecordsPerType
			policy.MaxScanRecords = policy.MaxRecordsPerType + 10
			policy.MaxTotalBytes = 1 << 30
			policy.MaxScanBytes = 1 << 30
			store := NewRecordsStore(root)
			for index := 0; index < existing; index++ {
				if _, err := store.Create("bench", map[string]any{"index": index, "value": "payload"}, policy); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := store.Create("bench", map[string]any{"index": -1, "value": "payload"}, policy); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
