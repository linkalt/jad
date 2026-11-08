package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordsQuotaSeesEveryRecordSizeAfterCreateUpdateAndOutsideChanges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	policy := recordsTestSettings(root)
	policy.MaxRecordBytes = 1 << 20
	policy.MaxTotalBytes = 3200
	policy.MaxScanBytes = 1 << 20
	store := NewRecordsStore(root)
	body := func(n int) map[string]any { return map[string]any{"v": strings.Repeat("x", n)} }

	first, err := store.Create("note", body(1000), policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("note", body(1000), policy); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("note", body(1500), policy); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("third create over the byte quota: err = %v, want capacity error", err)
	}
	// An update that grows a record is counted against the remembered size.
	if _, err := store.Update("note", first.Ref, "", body(2400), policy); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("growing update over the quota: err = %v, want capacity error", err)
	}
	updated, err := store.Update("note", first.Ref, "", body(1500), policy)
	if err != nil {
		t.Fatal(err)
	}
	// 1500 + 1000 is under the quota, but a further 900 is over it: the update's new size must count.
	if _, err := store.Create("note", body(900), policy); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("create after growing update: err = %v, want capacity error", err)
	}

	// A record deleted outside the store frees its quota on the next scan.
	if err := os.Remove(filepath.Join(root, updated.Ref)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("note", body(900), policy); err != nil {
		t.Fatalf("create after outside delete: %v", err)
	}

	// A second store on the same root sees the files; the first store sees a record the
	// second created (a new file name is always stat-ed).
	other := NewRecordsStore(root)
	if _, err := other.Create("note", body(1000), policy); err != nil {
		t.Fatalf("create through second store: %v", err)
	}
	if _, err := store.Create("note", body(1000), policy); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("create after another store filled the quota: err = %v, want capacity error", err)
	}
}

func TestRecordsTotalsMatchAFreshStoreAfterManyOperations(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	policy := recordsTestSettings(root)
	policy.MaxTotalBytes = 1 << 30
	policy.MaxScanBytes = 1 << 30
	policy.MaxTypes = 10
	policy.MaxRecordsPerType = 100
	policy.MaxTotalRecords = 1000
	policy.MaxScanRecords = 1000
	store := NewRecordsStore(root)
	var refs []string
	for index := 0; index < 40; index++ {
		item, err := store.Create([]string{"a", "b", "c"}[index%3], map[string]any{"n": index, "v": strings.Repeat("y", index*7)}, policy)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, item.Ref)
	}
	for index := 0; index < 40; index += 3 {
		parts := strings.SplitN(refs[index], "/", 2)
		if _, err := store.Update(parts[0], refs[index], "", map[string]any{"grown": strings.Repeat("z", 500+index)}, policy); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.totals("a", policy)
	if err != nil {
		t.Fatal(err)
	}
	want, err := NewRecordsStore(root).totals("a", policy)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cached totals = %+v, fresh store totals = %+v", got, want)
	}
}
