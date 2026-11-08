package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeNativeTaskSettingsDefaultsAndOverrides(t *testing.T) {
	cwd := t.TempDir()
	defaultSettings, err := normalizeNativeTaskSettings(map[string]json.RawMessage{}, "/session/setting.json", cwd, "")
	if err != nil {
		t.Fatalf("normalize defaults: %v", err)
	}
	if defaultSettings != nil {
		t.Fatalf("task settings without tools = %+v, want nil", defaultSettings)
	}

	settings, err := normalizeNativeTaskSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"task":{"max_tasks":12,"list_limit":2,"max_list_limit":3,"storage_dir":"plans","statecontext":{"statuses":["done","in-progress","done"],"limit":2}}}}`),
	}, "/session/setting.json", cwd, filepath.Join(cwd, "tool-data"))
	if err != nil {
		t.Fatalf("normalize configured task settings: %v", err)
	}
	if settings.MaxTasks != 12 || settings.ListLimit != 2 || settings.MaxListLimit != 3 || settings.MaxTitleChars != defaultNativeTaskMaxTitle {
		t.Fatalf("normalized task limits = %+v", settings)
	}
	if settings.MaxBatchOps != 20 {
		t.Fatalf("default max_batch_ops = %d, want 20", settings.MaxBatchOps)
	}
	if settings.StorageDir != filepath.Join(cwd, "plans") {
		t.Fatalf("task storage dir = %q", settings.StorageDir)
	}
	if len(settings.StateContext.Statuses) != 2 || settings.StateContext.Statuses[0] != "done" || settings.StateContext.Statuses[1] != "in-progress" || settings.StateContext.Limit != 2 {
		t.Fatalf("task statecontext settings = %+v", settings.StateContext)
	}

	for raw, want := range map[string]int64{`{"max_batch_ops":1}`: 1, `{"max_batch_ops":50}`: 50, `{"max_batch_ops":200}`: 200} {
		settings, err = normalizeNativeTaskSettings(map[string]json.RawMessage{
			"tools": json.RawMessage(`{"native":{"task":` + raw + `}}`),
		}, "/session/setting.json", cwd, "")
		if err != nil || settings.MaxBatchOps != want {
			t.Fatalf("task settings %s: max_batch_ops = %+v, error = %v, want %d", raw, settings, err, want)
		}
	}

	absolute := filepath.Join(cwd, "absolute-tasks")
	settings, err = normalizeNativeTaskSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"task":{"storage_dir":"` + absolute + `"}}}`),
	}, "/session/setting.json", cwd, "")
	if err != nil || settings.StorageDir != absolute {
		t.Fatalf("absolute storage_dir = %+v, error = %v", settings, err)
	}
}

func TestNormalizeNativeTaskSettingsRejectsInvalidPolicies(t *testing.T) {
	cwd := t.TempDir()
	for _, raw := range []string{
		`{"storage_path":"tasks.json"}`,
		`{"unknown":1}`,
		`{"max_tasks":0}`,
		`{"list_limit":501}`,
		`{"list_limit":3,"max_list_limit":2}`,
		`{"statecontext":null}`,
		`{"statecontext":{"statuses":["unknown"]}}`,
		`{"statecontext":{"limit":0}}`,
		`{"storage_dir":"  "}`,
		`{"max_batch_ops":0}`,
		`{"max_batch_ops":201}`,
		`{"max_batch_ops":"20"}`,
		`{"max_batch_ops":1.5}`,
	} {
		_, err := normalizeNativeTaskSettings(map[string]json.RawMessage{
			"tools": json.RawMessage(`{"native":{"task":` + raw + `}}`),
		}, "/session/setting.json", cwd, "")
		if err == nil || !strings.Contains(err.Error(), "tools.native.task") {
			t.Errorf("invalid task policy %s error = %v", raw, err)
		}
	}
}
