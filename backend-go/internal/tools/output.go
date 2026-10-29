package tools

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Status is the status field in the textual native-tool result envelope.
type Status string

const (
	StatusOK                  Status = "ok"
	StatusNotFound            Status = "not_found"
	StatusForbidden           Status = "forbidden"
	StatusInvalidInput        Status = "invalid_input"
	StatusUnsupportedEncoding Status = "unsupported_encoding"
	StatusBlocked             Status = "blocked"
	StatusError               Status = "error"
)

// Information preserves the insertion order used by the TypeScript renderer.
type Information struct {
	Key   string
	Value any
}

// Content optionally appends a bounded or streamed content section to a result.
type Content struct {
	Text     string
	Encoding string
}

// ResultOptions describes the stable text envelope returned by native tools.
type ResultOptions struct {
	ID          string
	Status      Status
	Summary     string
	Information []Information
	Content     *Content
}

func RenderToolResult(options ResultOptions) (string, error) {
	header, err := MarshalJSON(struct {
		ID      string `json:"id"`
		Status  Status `json:"status"`
		Summary string `json:"summary"`
	}{ID: options.ID, Status: options.Status, Summary: options.Summary})
	if err != nil {
		return "", err
	}
	lines := []string{string(header), "", "--- INFORMATION ---"}
	for _, item := range options.Information {
		lines = append(lines, item.Key+": "+valueText(item.Value))
	}
	if options.Content != nil {
		encoding := options.Content.Encoding
		if encoding != "base64" {
			encoding = "utf8"
		}
		token, err := contentToken(options.Content.Text)
		if err != nil {
			return "", err
		}
		lines = append(lines, "", fmt.Sprintf("--- CONTENT token=%s encoding=%s ---", token, encoding), options.Content.Text, fmt.Sprintf("--- END CONTENT token=%s ---", token))
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// AppendContent adds a content section to a result RenderToolResult produced without
// one, in the same layout RenderToolResult gives a result that has one.
func AppendContent(result string, content Content) (string, error) {
	encoding := content.Encoding
	if encoding != "base64" {
		encoding = "utf8"
	}
	token, err := contentToken(content.Text)
	if err != nil {
		return "", err
	}
	return result + fmt.Sprintf("\n--- CONTENT token=%s encoding=%s ---\n%s\n--- END CONTENT token=%s ---\n", token, encoding, content.Text, token), nil
}

func ExecutionError(id string, status Status, code, summary string, information ...Information) string {
	entries := []Information{{Key: "error_type", Value: "execution"}, {Key: "error_code", Value: code}}
	entries = append(entries, information...)
	result, err := RenderToolResult(ResultOptions{ID: id, Status: status, Summary: summary, Information: entries})
	if err != nil {
		return ""
	}
	return result
}

func SchemaError(id, summary string, issues any, example json.RawMessage) string {
	entries := []Information{{Key: "error_type", Value: "schema"}, {Key: "error_code", Value: "validation_failed"}}
	if len(example) > 0 {
		entries = append(entries, Information{Key: "example_operation", Value: example})
	}
	entries = append(entries, Information{Key: "ajv_errors", Value: issues})
	result, err := RenderToolResult(ResultOptions{ID: id, Status: StatusInvalidInput, Summary: summary, Information: entries})
	if err != nil {
		return ""
	}
	return result
}

func UnknownToolError(id, name string) string {
	return ExecutionError(id, StatusError, "unknown_tool", fmt.Sprintf("Tool %s is not available", name), Information{Key: "tool", Value: name})
}

func InvalidArgumentsJSONError(id, name string, example json.RawMessage) string {
	return SchemaError(id, fmt.Sprintf("Invalid JSON arguments for %s", name), []map[string]string{{
		"keyword": "parse",
		"message": "tool arguments must be valid JSON",
	}}, example)
}

func valueText(value any) string {
	if text, ok := value.(string); ok {
		if strings.Contains(text, "\n") {
			encoded, err := MarshalJSON(text)
			if err == nil {
				return string(encoded)
			}
		}
		return text
	}
	if value == nil {
		return "null"
	}
	if number, ok := value.(json.Number); ok {
		return number.String()
	}
	if boolean, ok := value.(bool); ok {
		return fmt.Sprint(boolean)
	}
	if stringer, ok := value.(fmt.Stringer); ok {
		return stringer.String()
	}
	encoded, err := MarshalJSON(value)
	if err != nil {
		return "null"
	}
	return string(encoded)
}

// MarshalJSON is the one marshaler for the bytes the model and messages.jsonl see: compact,
// with <, > and & left as they are, and the U+2028/U+2029 separators written raw inside
// strings, which is what JavaScript's JSON.stringify prints. Everything that reaches a
// json.RawMessage has to be encoded this way, because json.Marshal of a RawMessage would
// HTML-escape it again.
func MarshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return replaceJSONStringSeparators(bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})), nil
}

func contentToken(content string) (string, error) {
	for {
		var random [16]byte
		if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
			return "", fmt.Errorf("generate content token: %w", err)
		}
		token := "r_" + base64.RawURLEncoding.EncodeToString(random[:])
		if !strings.Contains(content, "--- END CONTENT token="+token+" ---") {
			return token, nil
		}
	}
}
