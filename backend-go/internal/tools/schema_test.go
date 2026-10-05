package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompileSchemaMatchesDraft7NativeKeywordsAndLocalRefs(t *testing.T) {
	validator, err := CompileSchema([]byte(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["add","get"]},
			"id":{"$ref":"#/definitions/nonEmpty"}
		},
		"required":["action"],
		"additionalProperties":false,
		"allOf":[{
			"if":{"properties":{"action":{"const":"get"}},"required":["action"]},
			"then":{"required":["id"]}
		}],
		"definitions":{"nonEmpty":{"type":"string","minLength":1}}
	}`))
	if err != nil {
		t.Fatalf("CompileSchema() error = %v", err)
	}

	issues, err := validator.Validate([]byte(`{"action":"get"}`))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("Validate() accepted an instance missing the conditional id")
	}
	foundRequired := false
	for _, issue := range issues {
		if issue.Keyword == "required" && issue.Params["missingProperty"] == "id" {
			foundRequired = true
			if issue.SchemaPath == "" || issue.InstancePath != "" {
				t.Fatalf("required issue paths = (%q, %q)", issue.SchemaPath, issue.InstancePath)
			}
		}
	}
	if !foundRequired {
		t.Fatalf("Validate() issues = %#v; missing required-property detail", issues)
	}

	valid, err := validator.Validate([]byte(`{"action":"get","id":"non-empty"}`))
	if err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}
	if len(valid) != 0 {
		t.Fatalf("Validate(valid) issues = %#v", valid)
	}

	badReference, err := validator.Validate([]byte(`{"action":"get","id":""}`))
	if err != nil {
		t.Fatalf("Validate(invalid referenced property) error = %v", err)
	}
	foundMinLength := false
	for _, issue := range badReference {
		if issue.Keyword == "minLength" {
			foundMinLength = true
			if !strings.HasSuffix(issue.SchemaPath, "/definitions/nonEmpty/minLength") {
				t.Fatalf("referenced schema path = %q", issue.SchemaPath)
			}
		}
	}
	if !foundMinLength {
		t.Fatalf("Validate(invalid referenced property) issues = %#v", badReference)
	}
}

func TestCompileSchemaRejectsExternalReferences(t *testing.T) {
	_, err := CompileSchema([]byte(`{"$ref":"file:///etc/passwd"}`))
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("CompileSchema() error = %v, want external reference rejection", err)
	}
}

func TestSchemaValidatorRejectsInvalidJSONAndSerializesAJVShapedIssues(t *testing.T) {
	validator, err := CompileSchema([]byte(`{
		"type":"object",
		"properties":{"name":{"type":"string"}},
		"required":["name"],
		"additionalProperties":false
	}`))
	if err != nil {
		t.Fatalf("CompileSchema() error = %v", err)
	}
	if _, err := validator.Validate([]byte(`{"name":`)); err == nil {
		t.Fatal("Validate() accepted malformed JSON")
	}

	issues, err := validator.Validate([]byte(`{"extra":true}`))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(issues) < 2 {
		t.Fatalf("Validate() issues = %#v, want missing and additional-property errors", issues)
	}
	for _, issue := range issues {
		if issue.Keyword == "required" && issue.Params["missingProperty"] == "name" {
			encoded, err := json.Marshal(issue)
			if err != nil {
				t.Fatalf("Marshal(issue) error = %v", err)
			}
			if !strings.Contains(string(encoded), `"instancePath":""`) || !strings.Contains(string(encoded), `"schemaPath":`) {
				t.Fatalf("serialized issue = %s, want AJV-shaped paths", encoded)
			}
			return
		}
	}
	t.Fatalf("Validate() issues = %#v, missing AJV-shaped required-property issue", issues)
}

func TestSchemaValidatorMapsDraft2020FalseItemsToAJVError(t *testing.T) {
	validator, err := CompileSchema([]byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"pair":{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"}],"items":false}},"required":["pair"]}`))
	if err != nil {
		t.Fatalf("CompileSchema() error = %v", err)
	}
	issues, err := validator.Validate([]byte(`{"pair":["word",42,true]}`))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	for _, issue := range issues {
		if issue.Keyword == "items" && issue.InstancePath == "/pair" && issue.SchemaPath == "#/properties/pair/items" && issue.Params["limit"] == 2 && issue.Message == "must NOT have more than 2 items" {
			return
		}
	}
	t.Fatalf("Validate() issues = %#v, want AJV-shaped items error", issues)
}

func TestUninitializedSchemaValidatorReturnsError(t *testing.T) {
	var validator *SchemaValidator
	if _, err := validator.Validate(json.RawMessage(`{}`)); err == nil {
		t.Fatal("Validate() on nil validator succeeded")
	}
}

func BenchmarkSchemaValidatorValidate(b *testing.B) {
	validator, err := CompileSchema([]byte(`{"type":"object","properties":{"path":{"type":"string","minLength":1}},"required":["path"],"additionalProperties":false}`))
	if err != nil {
		b.Fatal(err)
	}
	instance := []byte(`{"path":"src/main.go"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		issues, err := validator.Validate(instance)
		if err != nil || len(issues) != 0 {
			b.Fatalf("Validate() = (%#v, %v)", issues, err)
		}
	}
}
