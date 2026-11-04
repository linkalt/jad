// Package outputcheck checks a run's final answer against the JSON Schema of its output
// format. jad run, jad eval and the run route share it, so they judge an answer alike.
package outputcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"jad/backend-go/internal/tools"
)

// MaxReportedIssues is how many schema mismatches a verdict names (the rest are counted), and
// MaxReportedIssueBytes how long each path and message may be: both carry text from the answer.
const (
	MaxReportedIssues     = 5
	MaxReportedIssueBytes = 200
)

// Verdict is how an answer fared. Value is the answer as compact JSON when it matched; Error
// says why it did not, starting with "the final answer".
type Verdict struct {
	Valid bool            `json:"valid"`
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Check reads text as JSON (see JSONText) and validates it with validator.
func Check(validator *tools.SchemaValidator, text string) Verdict {
	answer := JSONText(text)
	fail := func(reason string) Verdict { return Verdict{Error: "the final answer " + reason} }
	if !json.Valid([]byte(answer)) {
		return fail("is not JSON")
	}
	issues, err := validator.Validate([]byte(answer))
	if err != nil {
		return fail("could not be checked against the output schema: " + err.Error())
	}
	if len(issues) > 0 {
		return fail("does not match the output schema: " + DescribeIssues(issues))
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(answer)); err != nil {
		return fail("is not JSON")
	}
	return Verdict{Valid: true, Value: compact.Bytes()}
}

// JSONText is the answer with surrounding space removed and, when the whole answer is one
// fenced code block (```json ... ```), the fence removed: a provider that does not constrain
// its output often wraps JSON that way.
func JSONText(text string) string {
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

// DescribeIssues names at most MaxReportedIssues mismatches, each cut to MaxReportedIssueBytes.
func DescribeIssues(issues []tools.ValidationIssue) string {
	parts := make([]string, 0, MaxReportedIssues+1)
	for index, issue := range issues {
		if index == MaxReportedIssues {
			parts = append(parts, fmt.Sprintf("and %d more", len(issues)-index))
			break
		}
		path := issue.InstancePath
		if path == "" {
			path = "/"
		}
		parts = append(parts, CutText(path, MaxReportedIssueBytes)+": "+CutText(issue.Message, MaxReportedIssueBytes))
	}
	return strings.Join(parts, "; ")
}

// CutText cuts text to about maximum bytes, at a rune boundary.
func CutText(text string, maximum int) string {
	if len(text) <= maximum {
		return text
	}
	cut := maximum
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}
