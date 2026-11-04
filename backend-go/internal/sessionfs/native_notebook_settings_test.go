package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestNotebookSettingsTakeTheEditSettings(t *testing.T) {
	edit := &NativeEditSettings{MaxBytes: 777}
	notebook := func(raw string) (*NativeNotebookSettings, error) {
		return normalizeNativeNotebookSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"notebook":` + raw + `}}`)}, edit, "s")
	}
	got, err := notebook(`{}`)
	if err != nil || got == nil || got.Edit.MaxBytes != 777 || got.MaxOutputChars != 2000 {
		t.Fatalf("defaults = %+v %v", got, err)
	}
	if got, err = notebook(`{"max_output_chars":0}`); err != nil || got.MaxOutputChars != 0 {
		t.Fatalf("zero = %+v %v", got, err)
	}
	if none, err := normalizeNativeNotebookSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{}}`)}, edit, "s"); none != nil || err != nil {
		t.Fatalf("absent = %+v %v", none, err)
	}
	for raw, want := range map[string]string{
		`{"paths":{}}`:                 "paths",
		`{"max_output_chars":-1}`:      "max_output_chars must be a safe integer between 0 and 1000000",
		`{"max_output_chars":1000001}`: "max_output_chars must be a safe integer between 0 and 1000000",
		`[]`:                           "notebook",
	} {
		if _, err := notebook(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", raw, err, want)
		}
	}
	_, err = normalizeNativeNotebookSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"notebook":{}}}`)}, nil, "s")
	if err == nil || !strings.Contains(err.Error(), "tools.native.notebook needs tools.native.edit") {
		t.Fatalf("without edit: %v", err)
	}
}

func TestNotebookSchemaAgreesWithTheLoader(t *testing.T) {
	schema := compileSchema(t, "setting.schema.json#/properties/tools/properties/native/properties/notebook")
	edit := &NativeEditSettings{MaxBytes: 1}
	for _, test := range []struct {
		raw    string
		accept bool
	}{
		{`{}`, true},
		{`{"max_output_chars":0}`, true},
		{`{"max_output_chars":1000000}`, true},
		{`{"max_output_chars":-1}`, false},
		{`{"max_output_chars":1.5}`, false},
		{`{"max_bytes":1}`, false},
	} {
		if got := schemaAccepts(t, schema, test.raw); got != test.accept {
			t.Errorf("schema accepts %s = %v, want %v", test.raw, got, test.accept)
		}
		_, err := normalizeNativeNotebookSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"notebook":` + test.raw + `}}`)}, edit, "s")
		if (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
}

func TestNotebookLoadsFromTheSessionSettings(t *testing.T) {
	for _, test := range []struct {
		native map[string]any
		want   string
	}{
		{map[string]any{"edit": map[string]any{"max_bytes": 4096}, "notebook": map[string]any{"max_output_chars": 50}}, ""},
		{map[string]any{"notebook": map[string]any{}}, "needs tools.native.edit"},
	} {
		fixture := layouttest.Write(t, layouttest.Spec{Native: test.native})
		config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
		if err != nil {
			t.Fatal(err)
		}
		location, err := Resolve(config.Roots, fixture.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadSessionSettings(location, "", "")
		if test.want != "" {
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if loaded.NativeNotebook == nil || loaded.NativeNotebook.MaxOutputChars != 50 || loaded.NativeNotebook.Edit.MaxBytes != 4096 {
			t.Fatalf("notebook = %+v", loaded.NativeNotebook)
		}
	}
}
