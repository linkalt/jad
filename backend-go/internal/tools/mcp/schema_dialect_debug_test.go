package mcp

import (
	"bytes"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestJSONSchemaV6HonorsDeclaredDraft2020(t *testing.T) {
	raw := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"pair":{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"}],"items":false}},"required":["pair"]}`)
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft7)
	if err := compiler.AddResource("memory://draft-test/schema.json", document); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("memory://draft-test/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if compiled.DraftVersion != 2020 {
		t.Fatalf("compiled draft = %d, want 2020", compiled.DraftVersion)
	}
	pair := compiled.Properties["pair"]
	if pair == nil || len(pair.PrefixItems) != 2 || pair.Items2020 == nil || pair.Items2020.Bool == nil || *pair.Items2020.Bool {
		t.Fatalf("compiled tuple schema = %+v", pair)
	}
	if err := compiled.Validate(map[string]any{"pair": []any{"word", int64(42), true}}); err == nil {
		t.Fatal("compiled Draft 2020-12 schema accepted an extra tuple item")
	}
}
