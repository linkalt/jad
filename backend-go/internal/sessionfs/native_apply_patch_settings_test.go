package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadApplyPatch(t *testing.T, raw string, toolStorage string) (*NativeApplyPatchSettings, error) {
	t.Helper()
	return normalizeNativeApplyPatchSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"apply_patch":` + raw + `}}`)}, "/jad", "setting.json", "/work", "/state", toolStorage)
}

func TestApplyPatchSettingsDefaultsAndOverrides(t *testing.T) {
	settings, err := loadApplyPatch(t, `{}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if settings.MaxBytes != 1<<20 || settings.MaxFiles != 50 || settings.MaxPatchBytes != 1<<20 || !settings.Fsync ||
		settings.BackupDir == nil || *settings.BackupDir != filepath.Join("/state", "backups") || settings.BackupMaxCount != 5 {
		t.Fatalf("defaults = %+v", settings)
	}
	settings, err = loadApplyPatch(t, `{"max_files":3,"max_patch_bytes":100,"max_bytes":200,"backup_dir":"bk","fsync":false}`, "/tools")
	if err != nil {
		t.Fatal(err)
	}
	if settings.MaxFiles != 3 || settings.MaxPatchBytes != 100 || settings.MaxBytes != 200 || settings.Fsync || *settings.BackupDir != filepath.Join("/tools", "bk") {
		t.Fatalf("overrides = %+v", settings)
	}
	none, err := normalizeNativeApplyPatchSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{}}`)}, "/jad", "setting.json", "/work", "/state", "")
	if err != nil || none != nil {
		t.Fatalf("absent apply_patch = %+v, %v", none, err)
	}
}

func TestApplyPatchSettingsRefuseBadValues(t *testing.T) {
	for raw, want := range map[string]string{
		`{"max_files":0}`:       "max_files",
		`{"max_files":1001}`:    "max_files",
		`{"max_patch_bytes":0}`: "max_patch_bytes",
		`{"max_bytes":0}`:       "max_bytes",
		`{"fsync":"yes"}`:       "fsync",
		`{"approval":"never"}`:  "approval",
		`{"replace_all":true}`:  "replace_all is not supported",
		`[]`:                    "apply_patch",
	} {
		if _, err := loadApplyPatch(t, raw, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want it to contain %q", raw, err, want)
		}
	}
}

func TestApplyPatchSettingsSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs       map[string]map[string]any `json:"$defs"`
		Properties struct {
			Tools struct {
				Properties struct {
					Native struct {
						Properties map[string]map[string]any `json:"properties"`
					} `json:"native"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	entry := schema.Properties.Tools.Properties.Native.Properties["apply_patch"]
	if entry == nil {
		t.Fatal("setting.schema.json has no tools.native.apply_patch")
	}
	keys := map[string]bool{}
	for key := range entry["properties"].(map[string]any) {
		keys[key] = true
	}
	for key := range schema.Defs["edit_style"]["properties"].(map[string]any) {
		keys[key] = true
	}
	for _, key := range nativeApplyPatchKeys {
		if !keys[key] {
			t.Errorf("schema has no apply_patch.%s", key)
		}
	}
	properties := entry["properties"].(map[string]any)
	for key, want := range map[string][3]float64{
		"max_bytes":       {1, float64(maxNativeToolBytes), float64(defaultNativeApplyPatchMaxBytes)},
		"max_files":       {1, float64(maxNativeApplyPatchMaxFiles), float64(defaultNativeApplyPatchMaxFiles)},
		"max_patch_bytes": {1, float64(maxNativeToolBytes), float64(defaultNativeApplyPatchMaxPatchBytes)},
	} {
		field := properties[key].(map[string]any)
		if field["minimum"] != want[0] || field["maximum"] != want[1] || field["default"] != want[2] {
			t.Errorf("apply_patch.%s schema = %v..%v default %v, loader %v", key, field["minimum"], field["maximum"], field["default"], want)
		}
	}
}
