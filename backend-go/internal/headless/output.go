package headless

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"jad/backend-go/internal/tools"
)

// maxReportedOutputIssues is how many schema mismatches the error of an invalid_output
// result names (the rest are counted), and maxReportedIssueBytes how long each path and
// message may be: both carry text from the answer.
const (
	maxReportedOutputIssues = 5
	maxReportedIssueBytes   = 200
)

// cutText cuts text to about maximum bytes, at a rune boundary.
func cutText(text string, maximum int) string {
	if len(text) <= maximum {
		return text
	}
	cut := maximum
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

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
	answer := outputJSONText(result.Text)
	fail := func(reason string) {
		result.Status = StatusInvalidOutput
		result.Error = "the final answer " + reason
	}
	if !json.Valid([]byte(answer)) {
		fail("is not JSON")
		return
	}
	issues, err := o.validator.Validate([]byte(answer))
	if err != nil {
		fail("could not be checked against the output schema: " + err.Error())
		return
	}
	if len(issues) > 0 {
		fail("does not match the output schema: " + describeIssues(issues))
		return
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(answer)); err != nil {
		fail("is not JSON")
		return
	}
	result.Output = compact.Bytes()
}

// outputJSONText is the answer with surrounding space removed and, when the whole answer is
// one fenced code block (```json ... ```), the fence removed: a provider that does not
// constrain its output often wraps JSON that way.
func outputJSONText(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") || len(trimmed) < 6 {
		return trimmed
	}
	inner := trimmed[3 : len(trimmed)-3]
	newline := strings.IndexByte(inner, '\n')
	if newline < 0 || strings.Contains(inner, "```") {
		return trimmed
	}
	if language := strings.TrimSpace(inner[:newline]); language != "" && !strings.EqualFold(language, "json") {
		return trimmed
	}
	return strings.TrimSpace(inner[newline+1:])
}

func describeIssues(issues []tools.ValidationIssue) string {
	parts := make([]string, 0, maxReportedOutputIssues+1)
	for index, issue := range issues {
		if index == maxReportedOutputIssues {
			parts = append(parts, fmt.Sprintf("and %d more", len(issues)-index))
			break
		}
		path := issue.InstancePath
		if path == "" {
			path = "/"
		}
		parts = append(parts, cutText(path, maxReportedIssueBytes)+": "+cutText(issue.Message, maxReportedIssueBytes))
	}
	return strings.Join(parts, "; ")
}
