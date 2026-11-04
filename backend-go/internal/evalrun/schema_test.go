package evalrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	schemaPath = filepath.Join("..", "..", "..", "sample", "schemes", "eval-suite.schema.json")
	samplePath = filepath.Join("..", "..", "..", "sample", "evals", "smoke", "suite.json")
)

// The schema documents the defaults the loader applies; they must be the same numbers.
func TestSuiteSchemaDefaultsMatchTheLoader(t *testing.T) {
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Default any `json:"default"`
			Maximum any `json:"maximum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"timeout_ms":             float64(DefaultRunTimeoutMS),
		"check_timeout_ms":       float64(DefaultCheckTimeoutMS),
		"check_output_max_bytes": float64(DefaultCheckOutputMaxBytes),
		"repeat":                 float64(DefaultRepeat),
		"parallel":               float64(DefaultParallel),
		"keep_workspaces":        DefaultKeepWorkspaces,
		"shell":                  []any{defaultShell[0], defaultShell[1]},
	} {
		if got := schema.Properties[key].Default; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema default %v, loader default %v", key, got, want)
		}
	}
	for key, want := range map[string]float64{"repeat": maxRepeat, "parallel": maxParallel, "check_output_max_bytes": maxCheckOutputMaxBytes} {
		if got := schema.Properties[key].Maximum; got != want {
			t.Errorf("%s: schema maximum %v, loader maximum %v", key, got, want)
		}
	}
}

// The sample suite is valid for both the schema and the loader, and the two agree on the
// suites the loader refuses for shape (not for paths on disk).
func TestSampleSuiteAndSchemaAgreeWithTheLoader(t *testing.T) {
	schema, err := jsonschema.NewCompiler().Compile(schemaPath)
	if err != nil {
		t.Fatalf("schema does not compile: %v", err)
	}
	accepts := func(text []byte) bool {
		var value any
		if err := json.Unmarshal(text, &value); err != nil {
			t.Fatal(err)
		}
		return schema.Validate(value) == nil
	}
	sample, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	if !accepts(sample) {
		t.Fatal("the schema refuses the sample suite")
	}
	if _, err := LoadSuite(samplePath); err != nil {
		t.Fatalf("the loader refuses the sample suite: %v", err)
	}
	dir := t.TempDir()
	for name, text := range map[string]string{
		"minimal":        `{"schema_version":1,"name":"s","cases":[{"name":"a","prompt":"p","expect_text":["x"]}]}`,
		"unknown key":    `{"schema_version":1,"name":"s","bogus":1,"cases":[{"name":"a","prompt":"p","expect_text":["x"]}]}`,
		"case unknown":   `{"schema_version":1,"name":"s","cases":[{"name":"a","prompt":"p","expect_text":["x"],"bogus":1}]}`,
		"cannot fail":    `{"schema_version":1,"name":"s","cases":[{"name":"a","prompt":"p"}]}`,
		"runner's cwd":   `{"schema_version":1,"name":"s","session":{"cwd":"/x"},"cases":[{"name":"a","prompt":"p","expect_text":["x"]}]}`,
		"keep value":     `{"schema_version":1,"name":"s","keep_workspaces":"x","cases":[{"name":"a","prompt":"p","expect_text":["x"]}]}`,
		"bad case name":  `{"schema_version":1,"name":"s","cases":[{"name":"a b","prompt":"p","expect_text":["x"]}]}`,
		"blank prompt":   `{"schema_version":1,"name":"s","cases":[{"name":"a","prompt":" ","expect_text":["x"]}]}`,
		"repeat over":    `{"schema_version":1,"name":"s","repeat":1001,"cases":[{"name":"a","prompt":"p","expect_text":["x"]}]}`,
		"version":        `{"schema_version":2,"name":"s","cases":[{"name":"a","prompt":"p","expect_text":["x"]}]}`,
		"category given": `{"schema_version":1,"name":"s","session":{"category":"bench"},"cases":[{"name":"a","prompt":"p","expect_text":["x"]}]}`,
	} {
		_, loadErr := ParseSuite([]byte(text), dir)
		if schemaOK, loaderOK := accepts([]byte(text)), loadErr == nil; schemaOK != loaderOK {
			t.Errorf("%s: schema accepts = %v, loader accepts = %v (%v)", name, schemaOK, loaderOK, loadErr)
		}
	}
}
