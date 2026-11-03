package webui

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/imagestore"
)

// The reference projection below is the code readToolOutput replaced, kept verbatim: it
// read each output up to five times (UI text as a list, then as a string, stored images,
// inline images). readToolOutput must give the same text, images and inline count.

func referenceToolOutputForUI(raw json.RawMessage) string {
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) == nil && parts != nil {
		lines := make([]string, 0, len(parts))
		for _, part := range parts {
			typeName := stringField(part["type"])
			switch typeName {
			case "input_text":
				lines = append(lines, stringField(part["text"]))
			case "input_image":
				lines = append(lines, "[MCP image content]")
			case "input_file":
				filename := stringField(part["filename"])
				if filename == "" {
					lines = append(lines, "[MCP file]")
				} else {
					lines = append(lines, "[MCP file: "+filename+"]")
				}
			default:
				if typeName == "" {
					typeName = "unknown"
				}
				lines = append(lines, "[MCP content: "+typeName+"]")
			}
		}
		return strings.Join(lines, "\n")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

func referenceStoredImageNames(raw json.RawMessage) []string {
	var parts []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var names []string
	for _, part := range parts {
		name, ok := strings.CutPrefix(part.ImageURL, imagestore.RefPrefix)
		if part.Type != "input_image" || !ok {
			continue
		}
		if _, valid := imagestore.ValidName(name); !valid || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

func referenceInlineImageCount(raw json.RawMessage) int {
	var parts []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return 0
	}
	count := 0
	for _, part := range parts {
		header, _, found := strings.Cut(part.ImageURL, ";base64,")
		mime, isData := strings.CutPrefix(header, "data:")
		if part.Type == "input_image" && found && isData && inlineImageTypes[mime] {
			count++
		}
	}
	return count
}

func referenceToolResultStatus(raw json.RawMessage) (string, bool) {
	var outputs []map[string]json.RawMessage
	if json.Unmarshal(raw, &outputs) == nil && outputs != nil {
		for _, output := range outputs {
			if stringField(output["text"]) == mcpToolErrorText {
				return "error", true
			}
		}
		return "success", true
	}
	return "", false
}

func TestReadToolOutputMatchesTheFormerProjection(t *testing.T) {
	stored := imagestore.RefPrefix + strings.Repeat("a", 64) + ".png"
	outputs := []string{
		`"plain text"`,
		`"{\"status\":\"ok\"}\nbody"`,
		`  "leading space"`,
		`""`,
		`[]`,
		` [ {"type":"input_text","text":"a"} ]`,
		`[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]`,
		`[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="},{"type":"input_image","image_url":"data:image/svg+xml;base64,AAAA"},{"type":"input_image","image_url":"https://x/y.png"}]`,
		`[{"type":"input_image","image_url":"` + stored + `"},{"type":"input_image","image_url":"` + stored + `"},{"type":"input_image","image_url":"` + imagestore.RefPrefix + `bad"}]`,
		`[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,AAAA"},{"type":"input_file"}]`,
		`[{"type":"other"},{},null,{"type":null,"text":null}]`,
		`[{"type":"input_text","text":"` + mcpToolErrorText + `"},{"type":"input_text","text":"why"}]`,
		// Lists the typed decode refuses: they take the per-field helpers.
		`[{"type":"input_text","text":5},{"type":"input_image","image_url":"` + stored + `"}]`,
		`[{"type":"input_file","filename":["x"]},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]`,
		`["a string entry"]`,
		`[1,2]`,
		`[{"type":"input_text","text":"a"}`,
		// Values that are neither a string nor a list.
		`null`,
		`42`,
		`{"type":"input_text","text":"object"}`,
		`true`,
		``,
		`not json`,
	}
	for _, output := range outputs {
		raw := json.RawMessage(output)
		got := readToolOutput(raw)
		if want := referenceToolOutputForUI(raw); got.text != want {
			t.Errorf("%s: text = %q, want %q", output, got.text, want)
		}
		if want := referenceStoredImageNames(raw); !reflect.DeepEqual(got.images, want) {
			t.Errorf("%s: images = %#v, want %#v", output, got.images, want)
		}
		if want := referenceInlineImageCount(raw); got.inlineImages != want {
			t.Errorf("%s: inline images = %d, want %d", output, got.inlineImages, want)
		}
		record, err := json.Marshal(map[string]any{"type": "function_call_output", "call_id": "c", "output": raw})
		if err != nil {
			// Not a JSON value: it cannot sit in a transcript record.
			continue
		}
		part, ok := ToolResultPart(record, 0)
		if !ok {
			t.Fatalf("%s: no tool result part", output)
		}
		if status, list := referenceToolResultStatus(raw); list && part.Status != status {
			t.Errorf("%s: live status = %q, want %q", output, part.Status, status)
		}
	}
}
