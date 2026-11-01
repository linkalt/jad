package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/tools"
)

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
