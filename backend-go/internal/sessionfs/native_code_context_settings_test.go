package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeNativeCodeContextSettingsDefaultsAndOverrides(t *testing.T) {
	cwd := t.TempDir()
	absent, err := normalizeNativeCodeContextSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"task":{}}}`),
	}, "", "/session/setting.json", cwd, "")
	if err != nil || absent != nil {
		t.Fatalf("code_context without its key = %+v, error = %v; want nil", absent, err)
	}

	defaults, err := normalizeNativeCodeContextSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"code_context":{}}}`),
	}, "", "/session/setting.json", cwd, filepath.Join(cwd, "tool-data"))
	if err != nil {
		t.Fatalf("normalize defaults: %v", err)
	}
	want := NativeCodeContextSettings{
		StorageDir:          filepath.Join(cwd, "tool-data", defaultNativeCodeContextStorageDir),
		MaxEntries:          defaultNativeCodeContextMaxEntries,
		MaxDescriptionChars: defaultNativeCodeContextMaxDescriptionChars,
		MaxRangeLines:       defaultNativeCodeContextMaxRangeLines,
		MaxFileBytes:        defaultNativeCodeContextMaxFileBytes,
		MaxOutputBytes:      defaultNativeCodeContextMaxOutputBytes,
		MaxStorageBytes:     defaultNativeCodeContextMaxStorageBytes,
		Relocate:            true,
		DefaultPriority:     defaultNativeCodeContextDefaultPriority,
		MaxFindResults:      defaultNativeCodeContextMaxFindResults,
		MaxScanTasks:        defaultNativeCodeContextMaxScanTasks,
		MaxBatchItems:       defaultNativeCodeContextMaxBatchItems,
	}
	if defaults.StorageDir != want.StorageDir || defaults.MaxEntries != want.MaxEntries || defaults.MaxDescriptionChars != want.MaxDescriptionChars ||
		defaults.MaxRangeLines != want.MaxRangeLines || defaults.MaxFileBytes != want.MaxFileBytes || defaults.MaxOutputBytes != want.MaxOutputBytes ||
		defaults.MaxStorageBytes != want.MaxStorageBytes || defaults.Relocate != want.Relocate || defaults.DefaultPriority != want.DefaultPriority ||
		defaults.MaxFindResults != want.MaxFindResults || defaults.MaxScanTasks != want.MaxScanTasks || defaults.MaxBatchItems != want.MaxBatchItems || len(defaults.Policy.Paths) != 0 || defaults.Policy.UnrestrictedPaths {
		t.Fatalf("defaults = %+v, want %+v", *defaults, want)
	}

	// Without a profile a relative directory is under the cwd.
	noProfile, err := normalizeNativeCodeContextSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"code_context":{"storage_dir":"ctx"}}}`),
	}, "", "/session/setting.json", cwd, "")
	if err != nil || noProfile.StorageDir != filepath.Join(cwd, "ctx") {
		t.Fatalf("storage_dir without a profile = %+v, error = %v", noProfile, err)
	}

	shared := filepath.Join(cwd, "shared")
	configured, err := normalizeNativeCodeContextSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"code_context":{"storage_dir":"` + shared + `","max_entries":7,"max_description_chars":80,"max_range_lines":30,"max_file_bytes":1000,"max_output_bytes":2000,"max_storage_bytes":3000,"relocate":false,"default_priority":2,"max_find_results":4,"max_scan_tasks":5,"max_batch_items":6,"paths":[{"action":"deny","path":"` + filepath.Join(cwd, "secret") + `"}]}}}`),
	}, "", "/session/setting.json", cwd, filepath.Join(cwd, "tool-data"))
	if err != nil {
		t.Fatalf("normalize configured: %v", err)
	}
	if configured.StorageDir != shared || configured.MaxEntries != 7 || configured.MaxDescriptionChars != 80 || configured.MaxRangeLines != 30 ||
		configured.MaxFileBytes != 1000 || configured.MaxOutputBytes != 2000 || configured.MaxStorageBytes != 3000 ||
		configured.Relocate || configured.DefaultPriority != 2 || configured.MaxFindResults != 4 || configured.MaxScanTasks != 5 || configured.MaxBatchItems != 6 {
		t.Fatalf("configured = %+v", *configured)
	}
	if len(configured.Policy.Paths) != 1 || configured.Policy.Paths[0].Action != "deny" {
		t.Fatalf("configured paths = %+v", configured.Policy)
	}
}

func TestNormalizeNativeCodeContextSettingsRejectsInvalidValues(t *testing.T) {
	cwd := t.TempDir()
	for _, raw := range []string{
		`[]`,
		`{"unknown":1}`,
		`{"storage_dir":"  "}`,
		`{"storage_dir":3}`,
		`{"max_entries":0}`,
		`{"max_entries":100001}`,
		`{"max_description_chars":0}`,
		`{"max_range_lines":0}`,
		`{"max_file_bytes":0}`,
		`{"max_file_bytes":10485761}`,
		`{"max_output_bytes":0}`,
		`{"max_storage_bytes":0}`,
		`{"paths":"everywhere"}`,
		`{"relocate":"yes"}`,
		`{"default_priority":0}`,
		`{"default_priority":7}`,
		`{"max_deliveries":3}`,
		`{"max_find_results":1001}`,
		`{"max_scan_tasks":0}`,
		`{"max_batch_items":0}`,
		`{"max_batch_items":1001}`,
	} {
		_, err := normalizeNativeCodeContextSettings(map[string]json.RawMessage{
			"tools": json.RawMessage(`{"native":{"code_context":` + raw + `}}`),
		}, "", "/session/setting.json", cwd, "")
		if err == nil || !strings.Contains(err.Error(), "tools.native.code_context") {
			t.Errorf("code_context %s error = %v, want one naming tools.native.code_context", raw, err)
		}
	}
}
