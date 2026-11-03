package webui

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// inlineImageTypes are the image types the browser is shown from a data URL; the same set
// the image store handles.
var inlineImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

// inlineImageURLs lists, in order, the base64 data URLs of the input_image parts of a
// tool output. Stored-image references and other URLs are not listed.
func inlineImageURLs(output json.RawMessage) []string {
	parts, ok := imageParts(output)
	if !ok {
		return nil
	}
	return inlineImageURLsOf(parts)
}

func inlineImageURLsOf(parts []toolOutputPart) []string {
	var urls []string
	for _, part := range parts {
		if part.Type != "input_image" {
			continue
		}
		header, _, found := strings.Cut(part.ImageURL, ";base64,")
		mime, isData := strings.CutPrefix(header, "data:")
		if found && isData && inlineImageTypes[mime] {
			urls = append(urls, part.ImageURL)
		}
	}
	return urls
}

// InlineImage decodes the index-th inline image of a function_call_output record. ok is
// false when the record has no such image, the payload is not base64, or its bytes are
// not the type the header claims.
func InlineImage(record json.RawMessage, index int) (data []byte, contentType string, ok bool) {
	var fields struct {
		Type   string          `json:"type"`
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(record, &fields) != nil || fields.Type != "function_call_output" {
		return nil, "", false
	}
	urls := inlineImageURLs(fields.Output)
	if index < 0 || index >= len(urls) {
		return nil, "", false
	}
	header, payload, _ := strings.Cut(urls[index], ";base64,")
	claimed := strings.TrimPrefix(header, "data:")
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || http.DetectContentType(data) != claimed {
		return nil, "", false
	}
	return data, claimed, true
}

// FindInlineImage decodes the index-th inline image of the tool result callID that joined
// the message messageID. The lookup is bound to that message, so a call id that repeats in
// another turn of the same records cannot serve that turn's image. callID is the id the
// message part reports, which is synthetic for an output that had none.
func FindInlineImage(records []TranscriptRecord, messageID, callID string, index int) (data []byte, contentType string, ok bool) {
	var source json.RawMessage
	// Only the asked tool result reads its output: the others are needed for grouping
	// alone, and a turn of large inline images would otherwise be decoded whole per fetch.
	_, err := normalizeTranscript(records, func(id string, part Part, entry TranscriptRecord) {
		if source == nil && id == messageID && part.CallID == callID && part.InlineImages > 0 {
			source = entry.Raw
		}
	}, func(id string) bool { return id == callID })
	if err != nil || source == nil {
		return nil, "", false
	}
	return InlineImage(source, index)
}

// HasInlineImages reports whether any part of the message still carries an inline image.
func HasInlineImages(message Message) bool { return hasInlineImages(message.Parts) }
