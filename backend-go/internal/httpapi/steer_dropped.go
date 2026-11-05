package httpapi

import (
	"bytes"
	"encoding/json"
	"strings"
)

// withoutInlineData returns inputs with the data of each inline image or file left out: an
// image_url or file_data that is a data URL keeps only its header ("data:image/png;base64,"),
// and an omitted_bytes field beside it says how many bytes were left out. A steer_dropped
// event goes to every reader's queue, and the sender of a dropped message holds its own copy
// of the data. An input that does not decode is kept as it is.
func withoutInlineData(inputs []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(inputs))
	for index, input := range inputs {
		out[index] = input
		if !bytes.Contains(input, []byte(`"data:`)) {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(input))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil {
			continue
		}
		if !dropInlineData(value) {
			continue
		}
		if encoded, err := json.Marshal(value); err == nil {
			out[index] = encoded
		}
	}
	return out
}

// dropInlineData cuts the data URLs of value in place and reports whether it cut one.
func dropInlineData(value any) bool {
	cut := false
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"image_url", "file_data"} {
			text, ok := typed[key].(string)
			if !ok || !strings.HasPrefix(text, "data:") {
				continue
			}
			header, data, found := strings.Cut(text, ",")
			if !found {
				continue
			}
			typed[key] = header + ","
			typed["omitted_bytes"] = len(data)
			cut = true
		}
		for _, child := range typed {
			cut = dropInlineData(child) || cut
		}
	case []any:
		for _, child := range typed {
			cut = dropInlineData(child) || cut
		}
	}
	return cut
}
