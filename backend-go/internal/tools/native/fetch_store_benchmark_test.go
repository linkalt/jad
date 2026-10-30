package native

import (
	"fmt"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// fetchStorePageDocument stores one document of about size bytes and returns its first cursor.
func fetchStorePageDocument(b *testing.B, size int) (*FetchStore, string) {
	b.Helper()
	store := NewFetchStore(b.TempDir())
	body := strings.Repeat("Repeated body text for fetch paging, with ünicode and 😀 emoji. ", size/64+1)[:size]
	document, err := store.Create(FetchDocument{URL: "https://example.com/", Markdown: body, Status: 200}, sessionfs.NativeFetchSettings{MaxStorageBytes: int64(size) * 4})
	if err != nil {
		b.Fatal(err)
	}
	return store, fetchCursor(document.ID, 0)
}

func BenchmarkFetchStorePageFirstPage(b *testing.B) {
	for _, size := range []int{1 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			store, cursor := fetchStorePageDocument(b, size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := store.Page(cursor, 10_000); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
