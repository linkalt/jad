package native

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func cacheTestStore(t *testing.T) (*FetchStore, *time.Time, FetchDocument) {
	t.Helper()
	store := NewFetchStore(t.TempDir())
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	policy := sessionfs.NativeFetchSettings{MaxStorageBytes: 1 << 20}
	document, err := store.Create(FetchDocument{URL: "https://a.test/", FinalURL: "https://a.test/x", Markdown: "hello world", Truncated: true}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return store, &clock, document
}

func TestFetchStoreCachedPageHitAndExpiry(t *testing.T) {
	store, clock, document := cacheTestStore(t)
	ttl := time.Minute
	store.rememberURLs([]string{"https://a.test/", "https://a.test/x"}, document.ID, ttl)

	page, ok := store.cachedPage("https://a.test/x", 5)
	if !ok || page.Content != "hello" || page.Document.ID != document.ID || !page.Document.Truncated {
		t.Fatalf("hit = %+v, %v", page, ok)
	}
	if page.NextCursor != fetchCursor(document.ID, 5) {
		t.Fatalf("next cursor = %q", page.NextCursor)
	}
	*clock = clock.Add(ttl)
	if _, ok := store.cachedPage("https://a.test/", 5); ok {
		t.Fatal("expired entry was served")
	}
	if _, present := store.urlIndex["https://a.test/"]; present {
		t.Fatal("expired entry was not dropped")
	}
}

func TestFetchStoreCachedPageMissesWhenFilesAreGone(t *testing.T) {
	store, _, document := cacheTestStore(t)
	store.rememberURLs([]string{"https://a.test/"}, document.ID, time.Minute)
	if err := os.Remove(filepath.Join(store.directory, document.ID+".md")); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.cachedPage("https://a.test/", 5); ok {
		t.Fatal("served a document whose file is gone")
	}
	if _, present := store.urlIndex["https://a.test/"]; present {
		t.Fatal("stale entry was kept")
	}
}

func TestFetchStoreRememberURLsSweepsExpiredEntries(t *testing.T) {
	store, clock, document := cacheTestStore(t)
	store.rememberURLs([]string{"https://old.test/"}, document.ID, time.Minute)
	*clock = clock.Add(2 * time.Minute)
	store.rememberURLs([]string{"https://new.test/"}, document.ID, time.Minute)
	if _, present := store.urlIndex["https://old.test/"]; present {
		t.Fatal("expired entry survived the sweep")
	}
	if _, present := store.urlIndex["https://new.test/"]; !present {
		t.Fatal("new entry missing")
	}
}

func TestFetchStoreTruncatedFlagPersists(t *testing.T) {
	store, _, document := cacheTestStore(t)
	page, err := store.Page(fetchCursor(document.ID, 6), 100)
	if err != nil || !page.Document.Truncated {
		t.Fatalf("page = %+v, %v", page, err)
	}
}

func TestFetchStoreCacheConcurrentAccess(t *testing.T) {
	store, _, document := cacheTestStore(t)
	var group sync.WaitGroup
	hits := make([]bool, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			url := "https://a.test/" + strconv.Itoa(i%3)
			store.rememberURLs([]string{url}, document.ID, time.Minute)
			page, ok := store.cachedPage(url, 5)
			hits[i] = ok && page.Document.ID == document.ID && page.Content == "hello"
		}(i)
	}
	group.Wait()
	for i, hit := range hits {
		if !hit {
			t.Fatalf("goroutine %d did not read back its own entry", i)
		}
	}
}
