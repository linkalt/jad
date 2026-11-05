package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"

	"jad/backend-go/internal/tools"
)

// outputFormatNamePattern is the name rule of a Responses json_schema format.
var outputFormatNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// defaultOutputFormatName names a format sent without a name; the Responses API requires one.
const defaultOutputFormatName = "output"

// runOutputFormat is the "output_format" member of a run request: a Responses
// text.format of type json_schema. Fields are written in this order.
type runOutputFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Strict      *bool           `json:"strict,omitempty"`
}

// parseRunOutputFormat checks the output_format member of a run request and returns it
// re-encoded, ready for text.format. null means none. The schema must compile, so a
// malformed one is refused before the run starts instead of by the provider mid-run.
func parseRunOutputFormat(raw []byte) (json.RawMessage, error) {
	if firstJSONByte(raw) == 'n' {
		return nil, nil
	}
	refuse := func(message string) error {
		return &StatusError{Code: http.StatusBadRequest, Message: message}
	}
	if firstJSONByte(raw) != '{' {
		return nil, refuse("output_format must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var format runOutputFormat
	if err := decoder.Decode(&format); err != nil {
		return nil, refuse("output_format must hold only type, name, description, schema and strict, with type, name and description strings and strict a boolean")
	}
	if format.Type != "json_schema" {
		return nil, refuse(`output_format.type must be "json_schema"`)
	}
	if format.Name == "" {
		format.Name = defaultOutputFormatName
	}
	if !outputFormatNamePattern.MatchString(format.Name) {
		return nil, refuse("output_format.name must be 1 to 64 letters, digits, underscores or hyphens")
	}
	if firstJSONByte(format.Schema) != '{' {
		return nil, refuse("output_format.schema must be a JSON Schema object")
	}
	if _, err := tools.CompileSchema(format.Schema); err != nil {
		return nil, refuse("output_format.schema is not a valid JSON Schema: " + err.Error())
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, format.Schema); err != nil {
		return nil, refuse("output_format.schema must be a JSON Schema object")
	}
	format.Schema = compact.Bytes()
	encoded, err := json.Marshal(format)
	if err != nil {
		return nil, refuse("output_format cannot be encoded")
	}
	return encoded, nil
}

// outputFormatValidator compiles the schema of a format parseRunOutputFormat accepted; nil for
// none. The completed event of the run carries the answer's verdict against it.
func outputFormatValidator(format json.RawMessage) *tools.SchemaValidator {
	if len(format) == 0 {
		return nil
	}
	var parsed runOutputFormat
	if json.Unmarshal(format, &parsed) != nil {
		return nil
	}
	validator, err := tools.CompileSchema(parsed.Schema)
	if err != nil {
		return nil
	}
	return validator
}
