package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

// self is named in the path description of both copies of the profile schema, and the schema accepts it.
func TestProfileSchemaNamesSelfInBothCopies(t *testing.T) {
	validator, err := tools.CompileSchema(profileInputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if issues, err := validator.Validate([]byte(`{"action":"info","path":"self"}`)); err != nil || len(issues) != 0 {
		t.Fatalf("path self was refused by the schema: %v %v", issues, err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []tools.Definition `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("profile.json: %v, %d tools", err, len(document.Tools))
	}
	copies := map[string]json.RawMessage{"in code": profileInputSchema, "sample": document.Tools[0].InputSchema}
	for name, schema := range copies {
		var parsed struct {
			Properties struct {
				Path struct {
					Description string `json:"description"`
				} `json:"path"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(schema, &parsed); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(parsed.Properties.Path.Description, "self") {
			t.Errorf("the %s path description does not name self: %q", name, parsed.Properties.Path.Description)
		}
	}
}

// The sample manifests of the profile and transcripts tools carry the same input schema as the
// Go code (the catalog refuses a mismatch); this pins both so an edit to one shows up here.
func TestProfileAndTranscriptsManifestsMatchTheGoSchemas(t *testing.T) {
	for _, tc := range []struct {
		file   string
		schema json.RawMessage
	}{
		{"profile.json", profileInputSchema},
		{"transcripts.json", transcriptsInputSchema},
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Tools []tools.Definition `json:"tools"`
		}
		if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
			t.Fatalf("%s: %v, %d tools", tc.file, err, len(document.Tools))
		}
		same, err := sameJSON(document.Tools[0].InputSchema, tc.schema)
		if err != nil || !same {
			t.Errorf("sample/tools/native/%s inputSchema differs from the in-code schema (err %v)", tc.file, err)
		}
	}
}
