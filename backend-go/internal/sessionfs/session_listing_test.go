package sessionfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSessionForListing(t *testing.T, root Root, id string, size int) {
	t.Helper()
	data := []byte(`{"cwd":"/x"}`)
	if size > 0 {
		data = make([]byte, size)
	}
	if err := os.WriteFile(filepath.Join(root.Directory, id+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListSessionFilesReportListsTheReadableAndNamesTheOversized(t *testing.T) {
	roots := sessionRoots(t, 1)
	writeSessionForListing(t, roots[0], "good", 0)
	writeSessionForListing(t, roots[0], "big", int(maxInternalJSONFileBytes)+1)
	listing, err := ListSessionFilesReport(roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Files) != 1 || listing.Files[0].ID != "good" || listing.Truncated {
		t.Fatalf("files = %+v truncated = %v", listing.Files, listing.Truncated)
	}
	if len(listing.Skipped) != 1 || listing.Skipped[0].ID != "big" || !strings.Contains(listing.Skipped[0].Error, "larger than") {
		t.Fatalf("skipped = %+v", listing.Skipped)
	}
	files, err := ListSessionFiles(roots)
	if err != nil || len(files) != 1 {
		t.Fatalf("ListSessionFiles = %+v, %v", files, err)
	}
}

func TestListSessionFilesStopsReadingAtTheBudget(t *testing.T) {
	roots := sessionRoots(t, 1)
	for _, id := range []string{"a", "b", "c", "d"} {
		writeSessionForListing(t, roots[0], id, 100)
	}
	files, truncated, err := listSessionFiles(roots, 250, func(Location, error) {})
	if err != nil || !truncated || len(files) != 3 {
		t.Fatalf("files = %d truncated = %v err = %v; want 3 read (the third crosses the budget), truncated", len(files), truncated, err)
	}
	files, truncated, err = listSessionFiles(roots, 1000, func(Location, error) {})
	if err != nil || truncated || len(files) != 4 {
		t.Fatalf("files = %d truncated = %v err = %v; want all 4", len(files), truncated, err)
	}
}

func TestSessionReadAndDeleteStayStrictForAnOversizedFile(t *testing.T) {
	roots := sessionRoots(t, 1)
	writeSessionForListing(t, roots[0], "big", int(maxInternalJSONFileBytes)+1)
	if _, err := ReadSessionFile(roots, "big"); err == nil {
		t.Fatal("ReadSessionFile read an oversized file")
	}
}
