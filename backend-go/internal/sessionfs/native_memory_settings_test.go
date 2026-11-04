package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNativeMemorySettingsDefaultsAndSelectors(t *testing.T) {
	cwd := t.TempDir()
	defaults, err := normalizeNativeMemorySettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"memory":{}}}`),
	}, "/session/setting.json", cwd, "")
	if err != nil {
		t.Fatalf("normalize default memory settings: %v", err)
	}
	if defaults.StorageDir != filepath.Join(cwd, ".memory") || defaults.MaxTypes != 64 || defaults.MaxRecordsPerType != 1_000 || defaults.MaxTotalRecords != 10_000 || defaults.ListLimit != 20 || defaults.MaxListLimit != 100 || defaults.MaxCursors != 1_000 || defaults.CursorTTLMS != 86_400_000 || defaults.MaxQueryBytes != 1024 {
		t.Fatalf("memory defaults = %+v", defaults)
	}

	settings, err := normalizeNativeMemorySettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"memory":{"storage_dir":"state/memory","allowed_types":["task","task","note"],"list_limit":2,"max_list_limit":3,"max_query_bytes":65536,"statecontext":[{"type":"task","filter":{"status":"open","project.name":"core"},"exclude_any":[{"archived":true}],"limit":2}]}}}`),
	}, "/session/setting.json", cwd, "")
	if err != nil {
		t.Fatalf("normalize configured memory settings: %v", err)
	}
	if settings.StorageDir != filepath.Join(cwd, "state", "memory") || settings.ListLimit != 2 || settings.MaxListLimit != 3 || settings.MaxQueryBytes != 65536 || !reflect.DeepEqual(settings.AllowedTypes, []string{"task", "note"}) {
		t.Fatalf("configured memory settings = %+v", settings)
	}
	if len(settings.StateContext) != 1 || settings.StateContext[0].Type != "task" || settings.StateContext[0].Limit != 2 || len(settings.StateContext[0].ExcludeAny) != 1 || settings.StateContext[0].Filter["status"] != "open" {
		t.Fatalf("memory statecontext selectors = %+v", settings.StateContext)
	}
}

func TestNormalizeNativeMemorySettingsUsesTypeScriptSelectorNormalization(t *testing.T) {
	cwd := t.TempDir()
	settings, err := normalizeNativeMemorySettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"memory":{"statecontext":[{"type":" task ","limit":1.0},{"type":"note","limit":1e1}]}}}`),
	}, "/session/setting.json", cwd, "")
	if err != nil {
		t.Fatalf("normalize memory statecontext: %v", err)
	}
	if len(settings.StateContext) != 2 || settings.StateContext[0].Type != "task" || settings.StateContext[0].Limit != 1 || settings.StateContext[1].Type != "note" || settings.StateContext[1].Limit != 10 {
		t.Fatalf("normalized statecontext = %+v, want trimmed types and integral JSON numbers", settings.StateContext)
	}
}

func TestNormalizeNativeMemorySettingsRejectsInvalidPolicies(t *testing.T) {
	cwd := t.TempDir()
	for _, raw := range []string{
		`{"storage_path":"memory"}`,
		`{"list_limit":4,"max_list_limit":3}`,
		`{"max_record_bytes":100,"max_total_bytes":10}`,
		`{"max_query_bytes":0}`,
		`{"max_query_bytes":65537}`,
		`{"allowed_types":["../escape"]}`,
		`{"allowed_types":null}`,
		`{"statecontext":null}`,
		`{"statecontext":[{"type":"other"}],"allowed_types":["task"]}`,
		`{"statecontext":[{"type":"task","filter":{"a.b.c.d.e.f.g.h.i":1}}]}`,
		`{"statecontext":[{"type":"task","group_by":3}]}`,
		`{"statecontext":[{"type":"task","group_by":"a..b"}]}`,
		`{"statecontext":[{"type":"task","group_by":"a.b.c.d.e.f.g.h.i"}]}`,
		`{"statecontext":[{"type":"task","group_by":"__proto__"}]}`,
		`{"statecontext":[{"type":"task","group_by":"status","limit":3}]}`,
		`{"statecontext":[{"type":"task","group_by":"status","filter":{"status":"open"}}]}`,
	} {
		_, err := normalizeNativeMemorySettings(map[string]json.RawMessage{
			"tools": json.RawMessage(`{"native":{"memory":` + raw + `}}`),
		}, "/session/setting.json", cwd, "")
		if err == nil || !strings.Contains(err.Error(), "tools.native.memory") {
			t.Errorf("normalize invalid memory policy %s error = %v", raw, err)
		}
	}
}

func TestNormalizeNativeMemorySettingsAcceptsGroupBy(t *testing.T) {
	settings, err := normalizeNativeMemorySettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"memory":{"statecontext":[{"type":"bug_report","group_by":"meta.status","filter":{"area":"core"}},{"type":"note","group_by":null}]}}}`),
	}, "/session/setting.json", t.TempDir(), "")
	if err != nil {
		t.Fatalf("normalize group_by: %v", err)
	}
	if len(settings.StateContext) != 2 || settings.StateContext[0].GroupBy != "meta.status" || settings.StateContext[1].GroupBy != "" {
		t.Fatalf("group_by selectors = %+v", settings.StateContext)
	}
}
