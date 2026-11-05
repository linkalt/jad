package headless

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"jad/backend-go/internal/outputcheck"
	"jad/backend-go/internal/tools"
)

// maxReportedOutputIssues is how many schema mismatches an invalid_output error names.
const maxReportedOutputIssues = outputcheck.MaxReportedIssues

// OutputSchema asks a headless run for a final answer that is JSON matching Schema.
type OutputSchema struct {
	// Schema is a JSON Schema object.
	Schema json.RawMessage
	// Name names the format for the provider; empty lets the server choose.
	Name string
	// Strict asks the provider to enforce the schema exactly (OpenAI strict mode, which
	// needs every property required and additionalProperties false on every object).
	Strict bool

	validator *tools.SchemaValidator
}

// NewOutputSchema compiles schema, so a malformed one is refused before any run starts.
func NewOutputSchema(schema json.RawMessage, name string, strict bool) (*OutputSchema, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(schema), []byte("{")) {
		return nil, errors.New("the output schema must be a JSON object")
	}
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		return nil, fmt.Errorf("the output schema is not a valid JSON Schema: %w", err)
	}
	return &OutputSchema{Schema: schema, Name: name, Strict: strict, validator: validator}, nil
}

// format is the run request's output_format member.
func (o *OutputSchema) format() (json.RawMessage, error) {
	if o.validator == nil {
		return nil, errors.New("the output schema was not made with NewOutputSchema")
	}
	strict := o.Strict
	return json.Marshal(struct {
		Type   string          `json:"type"`
		Name   string          `json:"name,omitempty"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict"`
	}{"json_schema", o.Name, o.Schema, &strict})
}

// check reads the completed result's text as JSON and validates it. A match sets Output;
// anything else turns the result into invalid_output, with the reason in Error.
func (o *OutputSchema) check(result *Result) {
	o.apply(result, outputcheck.Check(o.validator, result.Text))
}

// apply sets the result from a verdict on its answer.
func (o *OutputSchema) apply(result *Result, verdict outputcheck.Verdict) {
	if !verdict.Valid {
		result.Status = StatusInvalidOutput
		result.Error = verdict.Error
		return
	}
	result.Output = verdict.Value
}

func outputJSONText(text string) string { return outputcheck.JSONText(text) }

func cutText(text string, maximum int) string { return outputcheck.CutText(text, maximum) }
