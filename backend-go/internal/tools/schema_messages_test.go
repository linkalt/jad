package tools

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The expected messages and params below were produced by AJV (the TypeScript
// backend's validator) for the same schema and instance, so a drift in Go's
// wording shows up here rather than in a model-visible tool error.
func TestSchemaValidatorMatchesAJVMessagesAndParams(t *testing.T) {
	cases := []struct {
		name     string
		schema   string
		instance string
		keyword  string
		message  string
		params   string
	}{
		{"minLength", `{"type":"string","minLength":2}`, `""`, "minLength", "must NOT have fewer than 2 characters", `{"limit":2}`},
		{"maxLength", `{"type":"string","maxLength":2}`, `"abc"`, "maxLength", "must NOT have more than 2 characters", `{"limit":2}`},
		{"minimum", `{"type":"integer","minimum":1}`, `0`, "minimum", "must be >= 1", `{"comparison":">=","limit":1}`},
		{"maximum", `{"type":"integer","maximum":1}`, `5`, "maximum", "must be <= 1", `{"comparison":"<=","limit":1}`},
		{"exclusiveMinimum", `{"type":"number","exclusiveMinimum":1}`, `1`, "exclusiveMinimum", "must be > 1", `{"comparison":">","limit":1}`},
		{"exclusiveMaximum", `{"type":"number","exclusiveMaximum":1}`, `1`, "exclusiveMaximum", "must be < 1", `{"comparison":"<","limit":1}`},
		{"fractional bound", `{"type":"number","minimum":0.5}`, `0`, "minimum", "must be >= 0.5", `{"comparison":">=","limit":0.5}`},
		{"pattern", `{"type":"string","pattern":"^a"}`, `"b"`, "pattern", `must match pattern "^a"`, `{"pattern":"^a"}`},
		{"enum", `{"enum":["a","b"]}`, `"c"`, "enum", "must be equal to one of the allowed values", `{"allowedValues":["a","b"]}`},
		{"const", `{"const":"a"}`, `"b"`, "const", "must be equal to constant", `{"allowedValue":"a"}`},
		{"minItems", `{"type":"array","minItems":2}`, `[1]`, "minItems", "must NOT have fewer than 2 items", `{"limit":2}`},
		{"maxItems", `{"type":"array","maxItems":1}`, `[1,2]`, "maxItems", "must NOT have more than 1 items", `{"limit":1}`},
		{"uniqueItems", `{"type":"array","uniqueItems":true}`, `[1,1]`, "uniqueItems", "must NOT have duplicate items (items ## 0 and 1 are identical)", `{"i":1,"j":0}`},
		{"minProperties", `{"type":"object","minProperties":1}`, `{}`, "minProperties", "must NOT have fewer than 1 properties", `{"limit":1}`},
		{"maxProperties", `{"type":"object","maxProperties":0}`, `{"a":1}`, "maxProperties", "must NOT have more than 0 properties", `{"limit":0}`},
		{"multipleOf", `{"type":"integer","multipleOf":2}`, `3`, "multipleOf", "must be multiple of 2", `{"multipleOf":2}`},
		{"type", `{"type":"object","properties":{"a":{"type":"string"}}}`, `{"a":1}`, "type", "must be string", `{"type":"string"}`},
		{"type union", `{"type":["string","null"]}`, `1`, "type", "must be string,null", `{"type":["string","null"]}`},
		{"not", `{"not":{"type":"string"}}`, `"x"`, "not", "must NOT be valid", `{}`},
		{"oneOf", `{"oneOf":[{"type":"string"},{"type":"string"}]}`, `"x"`, "oneOf", "must match exactly one schema in oneOf", `{"passingSchemas":[0,1]}`},
		{"propertyNames", `{"type":"object","propertyNames":{"maxLength":1}}`, `{"ab":1}`, "propertyNames", "property name must be valid", `{"propertyName":"ab"}`},
		{"contains", `{"type":"array","contains":{"type":"string"}}`, `[1]`, "contains", "must contain at least 1 valid item(s)", `{"minContains":1}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			validator, err := CompileSchema([]byte(test.schema))
			if err != nil {
				t.Fatalf("CompileSchema() error = %v", err)
			}
			issues, err := validator.Validate([]byte(test.instance))
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			for _, issue := range issues {
				if issue.Keyword != test.keyword {
					continue
				}
				// Tool output renders params without HTML escaping, so compare that form.
				var encoded bytes.Buffer
				encoder := json.NewEncoder(&encoded)
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(issue.Params); err != nil {
					t.Fatal(err)
				}
				params := bytes.TrimSpace(encoded.Bytes())
				if issue.Message != test.message || string(params) != test.params {
					t.Fatalf("%s issue = message %q params %s; AJV reports message %q params %s", test.keyword, issue.Message, params, test.message, test.params)
				}
				return
			}
			t.Fatalf("Validate() issues = %#v, missing keyword %q", issues, test.keyword)
		})
	}
}
