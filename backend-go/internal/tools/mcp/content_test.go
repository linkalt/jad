package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestContentPartsMapsMultimodalMCPResults(t *testing.T) {
	result := &mcpSDK.CallToolResult{
		IsError: true,
		Content: []mcpSDK.Content{
			&mcpSDK.TextContent{Text: "hello"},
			&mcpSDK.ImageContent{MIMEType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
			&mcpSDK.AudioContent{MIMEType: "audio/wav", Data: []byte{1, 2}},
			&mcpSDK.EmbeddedResource{Resource: &mcpSDK.ResourceContents{URI: "file:///tmp/guide.txt", Text: "resource text"}},
			&mcpSDK.EmbeddedResource{Resource: &mcpSDK.ResourceContents{URI: "file:///tmp/guide.pdf?download=1", MIMEType: "application/pdf", Blob: []byte("%PDF")}},
			&mcpSDK.ResourceLink{Name: "linked resource", URI: "file:///tmp/item", MIMEType: "text/plain"},
		},
		StructuredContent: map[string]any{"status": "ok"},
	}
	parts := contentParts(result)
	if len(parts) != 8 {
		t.Fatalf("content part count = %d, want 8: %#v", len(parts), parts)
	}
	if parts[0].Text == nil || *parts[0].Text != "MCP server reported a tool error." {
		t.Fatalf("error marker = %+v", parts[0])
	}
	if parts[1].Type != "input_text" || parts[1].Text == nil || *parts[1].Text != "hello" {
		t.Fatalf("text content = %+v", parts[1])
	}
	if parts[2].Type != "input_image" || parts[2].ImageURL == nil || !strings.HasPrefix(*parts[2].ImageURL, "data:image/png;base64,") || parts[2].Detail != "auto" {
		t.Fatalf("image content = %+v", parts[2])
	}
	if parts[3].Text == nil || !strings.Contains(*parts[3].Text, "audio input is not enabled") {
		t.Fatalf("audio content = %+v", parts[3])
	}
	if parts[4].Text == nil || *parts[4].Text != "Resource file:///tmp/guide.txt:\nresource text" {
		t.Fatalf("text resource = %+v", parts[4])
	}
	if parts[5].Type != "input_file" || parts[5].FileData == nil || parts[5].Filename != "guide.pdf" {
		t.Fatalf("binary resource = %+v", parts[5])
	}
	if parts[6].Text == nil || *parts[6].Text != "linked resource (text/plain): file:///tmp/item" {
		t.Fatalf("resource link = %+v", parts[6])
	}
	if parts[7].Text == nil || !strings.HasPrefix(*parts[7].Text, "Structured content:\n") {
		t.Fatalf("structured content = %+v", parts[7])
	}
}

func TestMarshalPartsMatchesJavaScriptJSONEscaping(t *testing.T) {
	text := "<>&\u2028\u2029\\u2028"
	encoded, err := marshalParts([]outputPart{textPart(text)})
	if err != nil {
		t.Fatalf("marshalParts() error = %v", err)
	}
	want := "[{\"type\":\"input_text\",\"text\":\"<>&\u2028\u2029\\\\u2028\"}]"
	if string(encoded) != want {
		t.Fatalf("marshalParts() = %q, want JavaScript JSON.stringify output %q", encoded, want)
	}
}

func TestBoundedContentTruncatesUTF8AndIncludesOmissionNotice(t *testing.T) {
	const maxBytes = 512
	result := &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: strings.Repeat("π", 10_000)}}}
	parts := boundedContent(result, maxBytes)
	if len(parts) != 2 || parts[0].Text == nil || parts[1].Text == nil || !strings.Contains(*parts[1].Text, "Additional MCP result content omitted") {
		t.Fatalf("bounded content = %#v", parts)
	}
	used := 0
	for _, part := range parts {
		used += partSize(part)
		if part.Text != nil && !utf8.ValidString(*part.Text) {
			t.Fatalf("truncated text is not valid UTF-8: %q", *part.Text)
		}
	}
	if used > maxBytes {
		t.Fatalf("bounded output size = %d, exceeds %d", used, maxBytes)
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("Marshal(parts) error = %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("bounded output encoded to empty JSON")
	}
}

func TestBoundedContentProvidesFallbackForEmptyAndInvalidResults(t *testing.T) {
	for name, result := range map[string]*mcpSDK.CallToolResult{
		"empty": {},
		"nil":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			parts := boundedContent(result, 1024)
			if len(parts) != 1 || parts[0].Text == nil {
				t.Fatalf("fallback content = %#v", parts)
			}
			if name == "empty" && *parts[0].Text != "MCP tool returned no content." {
				t.Fatalf("empty-result message = %q", *parts[0].Text)
			}
			if name == "nil" && *parts[0].Text != "MCP server returned an invalid tool result." {
				t.Fatalf("nil-result message = %q", *parts[0].Text)
			}
		})
	}
}

// Structured content that fits max_result_bytes is reordered the way JavaScript lists
// object keys; a larger payload is kept as the server sent it, because the content parts
// are cut to that limit anyway.
func TestStructuredContentForResultOrdersOnlyWhatFitsTheLimit(t *testing.T) {
	raw := json.RawMessage(`{"b":"value","2":"two","1":"one"}`)
	content, err := structuredContentForResult(raw, len(raw))
	if err != nil {
		t.Fatalf("structuredContentForResult() error = %v", err)
	}
	ordered, err := compactJSON(content)
	if err != nil {
		t.Fatalf("compactJSON() error = %v", err)
	}
	if want := `{"1":"one","2":"two","b":"value"}`; ordered != want {
		t.Fatalf("structured content = %s, want %s", ordered, want)
	}

	content, err = structuredContentForResult(raw, len(raw)-1)
	if err != nil {
		t.Fatalf("structuredContentForResult() error = %v", err)
	}
	if got, ok := content.(json.RawMessage); !ok || string(got) != string(raw) {
		t.Fatalf("structured content over the limit = %#v, want the raw %s", content, raw)
	}
}

// boundedPartBytes is the encoded size of a bounded result, which is what max_result_bytes
// limits.
func boundedPartBytes(parts []outputPart) int {
	total := 0
	for _, part := range parts {
		total += partSize(part)
	}
	return total
}

// An isError result used to keep only the canned marker once it exceeded
// max_result_bytes, so the server's message never reached the model.
func TestBoundedContentKeepsTheErrorTextAboveTheLimit(t *testing.T) {
	const maxBytes = 2048
	message := strings.Repeat("the server failed: detail; ", 220)
	parts := boundedContent(&mcpSDK.CallToolResult{IsError: true, Content: []mcpSDK.Content{
		&mcpSDK.TextContent{Text: message},
	}}, maxBytes)
	if parts[0].Text == nil || *parts[0].Text != "MCP server reported a tool error." {
		t.Fatalf("first part = %#v", parts[0])
	}
	kept := ""
	for _, part := range parts[1:] {
		if part.Text != nil && strings.HasPrefix(*part.Text, "the server failed: detail; ") {
			kept = *part.Text
		}
	}
	if len(kept) < 1000 {
		t.Fatalf("error text kept %d bytes of %d, parts = %#v", len(kept), len(message), parts)
	}
	if total := boundedPartBytes(parts); total > maxBytes {
		t.Fatalf("bounded output size = %d, exceeds %d", total, maxBytes)
	}
	if last := parts[len(parts)-1].Text; last == nil || !strings.Contains(*last, "Additional MCP result content omitted") {
		t.Fatalf("no omission notice: %#v", parts)
	}
}

// A part past the budget is cut instead of being dropped whole, so a short first part no
// longer costs the model everything after it.
func TestBoundedContentCutsThePartPastTheBudgetInsteadOfDroppingIt(t *testing.T) {
	const maxBytes = 2048
	long := strings.Repeat("abcdefgh", 400)
	parts := boundedContent(&mcpSDK.CallToolResult{Content: []mcpSDK.Content{
		&mcpSDK.TextContent{Text: "hi"},
		&mcpSDK.TextContent{Text: long},
	}}, maxBytes)
	if len(parts) != 3 {
		t.Fatalf("parts = %#v", parts)
	}
	if parts[0].Text == nil || *parts[0].Text != "hi" {
		t.Fatalf("first part = %#v", parts[0])
	}
	if parts[1].Text == nil || !strings.HasPrefix(*parts[1].Text, "abcdefgh") || len(*parts[1].Text) < 1000 {
		t.Fatalf("second part = %#v", parts[1])
	}
	if total := boundedPartBytes(parts); total > maxBytes {
		t.Fatalf("bounded output size = %d, exceeds %d", total, maxBytes)
	}
}

// The truncation used to size the kept text as budget/6, to allow for the worst case of
// \u00XX expansions, and so spent about a sixth of the budget.
func TestBoundedContentUsesMostOfTheBudgetForOneLongText(t *testing.T) {
	const maxBytes = 2048
	parts := boundedContent(&mcpSDK.CallToolResult{Content: []mcpSDK.Content{
		&mcpSDK.TextContent{Text: strings.Repeat("x", 4000)},
	}}, maxBytes)
	total := boundedPartBytes(parts)
	if total > maxBytes {
		t.Fatalf("bounded output size = %d, exceeds %d", total, maxBytes)
	}
	if total < maxBytes*3/4 {
		t.Fatalf("bounded output size = %d, less than three quarters of %d", total, maxBytes)
	}
}
