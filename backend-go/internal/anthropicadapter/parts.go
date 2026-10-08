package anthropicadapter

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// responsesPart is one content part of a message or of a tool output.
type responsesPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Refusal  string `json:"refusal"`
	ImageURL string `json:"image_url"`
	FileData string `json:"file_data"`
	FileURL  string `json:"file_url"`
	FileID   string `json:"file_id"`
	Filename string `json:"filename"`
}

// convertParts converts message content: a string or an array of parts.
func convertParts(param string, raw json.RawMessage) ([]anthropicBlock, *apiError) {
	parts, apiErr := decodeParts(param, raw)
	if apiErr != nil {
		return nil, apiErr
	}
	blocks := make([]anthropicBlock, 0, len(parts))
	for index, part := range parts {
		block, keep, apiErr := convertPart(fmt.Sprintf("%s[%d]", param, index), &part)
		if apiErr != nil {
			return nil, apiErr
		}
		if keep {
			blocks = append(blocks, block)
		}
	}
	return blocks, nil
}

// convertTextParts is convertParts for system text: only text is meaningful.
func convertTextParts(param string, raw json.RawMessage) ([]anthropicBlock, *apiError) {
	blocks, apiErr := convertParts(param, raw)
	if apiErr != nil {
		return nil, apiErr
	}
	for _, block := range blocks {
		if block.Type != "text" {
			return nil, badRequest(param, "system and developer messages can only contain text")
		}
	}
	return blocks, nil
}

// convertResultContent converts a function_call_output "output" value.
func convertResultContent(param string, raw json.RawMessage) ([]anthropicBlock, *apiError) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, nil
	}
	return convertParts(param, raw)
}

func decodeParts(param string, raw json.RawMessage) ([]responsesPart, *apiError) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []responsesPart{{Type: "input_text", Text: text}}, nil
	}
	var parts []responsesPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, badRequest(param, "content must be a string or an array of parts")
	}
	return parts, nil
}

// convertPart converts one part; keep is false for an empty text part, which
// Anthropic rejects.
func convertPart(param string, part *responsesPart) (anthropicBlock, bool, *apiError) {
	switch part.Type {
	case "input_text", "output_text", "text":
		return anthropicBlock{Type: "text", Text: part.Text}, part.Text != "", nil
	case "refusal":
		return anthropicBlock{Type: "text", Text: part.Refusal}, part.Refusal != "", nil
	case "input_image":
		block, apiErr := imageBlock(param, part)
		return block, apiErr == nil, apiErr
	case "input_file":
		block, apiErr := fileBlock(param, part)
		return block, apiErr == nil, apiErr
	default:
		return anthropicBlock{}, false, unsupported(param, "content part type %q is not supported", part.Type)
	}
}

func imageBlock(param string, part *responsesPart) (anthropicBlock, *apiError) {
	if part.ImageURL == "" {
		if part.FileID != "" {
			return anthropicBlock{}, unsupported(param, "file_id images are not supported, send image_url")
		}
		return anthropicBlock{}, badRequest(param, "input_image needs image_url")
	}
	if mediaType, data, ok := splitDataURL(part.ImageURL); ok {
		if !strings.HasPrefix(mediaType, "image/") {
			return anthropicBlock{}, badRequest(param, "image data URL has media type %q", mediaType)
		}
		return anthropicBlock{Type: "image", Source: &anthropicSource{Type: "base64", MediaType: mediaType, Data: data}}, nil
	}
	if !strings.HasPrefix(part.ImageURL, "http://") && !strings.HasPrefix(part.ImageURL, "https://") {
		return anthropicBlock{}, badRequest(param, "image_url must be a data URL or an http(s) URL")
	}
	return anthropicBlock{Type: "image", Source: &anthropicSource{Type: "url", URL: part.ImageURL}}, nil
}

func fileBlock(param string, part *responsesPart) (anthropicBlock, *apiError) {
	if part.FileID != "" && part.FileData == "" && part.FileURL == "" {
		return anthropicBlock{}, unsupported(param, "file_id files are not supported, send file_data or file_url")
	}
	if part.FileURL != "" {
		return anthropicBlock{Type: "document", Title: part.Filename,
			Source: &anthropicSource{Type: "url", URL: part.FileURL}}, nil
	}
	if part.FileData == "" {
		return anthropicBlock{}, badRequest(param, "input_file needs file_data or file_url")
	}
	mediaType, data := part.FileData, part.FileData
	fromURL := false
	if mt, d, ok := splitDataURL(part.FileData); ok {
		mediaType, data, fromURL = mt, d, true
	}
	if !fromURL {
		mediaType = guessMediaType(part.Filename, data)
	}
	switch {
	case mediaType == "application/pdf":
		return anthropicBlock{Type: "document", Title: part.Filename,
			Source: &anthropicSource{Type: "base64", MediaType: mediaType, Data: data}}, nil
	case strings.HasPrefix(mediaType, "text/"):
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return anthropicBlock{}, badRequest(param, "file_data is not valid base64")
		}
		return anthropicBlock{Type: "document", Title: part.Filename,
			Source: &anthropicSource{Type: "text", MediaType: "text/plain", Data: string(decoded)}}, nil
	default:
		return anthropicBlock{}, unsupported(param, "files of type %q are not supported, only PDF and text", mediaType)
	}
}

// splitDataURL splits "data:<type>;base64,<data>".
func splitDataURL(value string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(value, "data:")
	if !found {
		return "", "", false
	}
	header, data, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	mediaType, encoding, _ := strings.Cut(header, ";")
	if encoding != "base64" {
		return "", "", false
	}
	return mediaType, data, true
}

// guessMediaType picks a type for raw base64 file data, which carries none:
// the filename extension first, then the PDF magic bytes.
func guessMediaType(filename, data string) string {
	switch strings.ToLower(path.Ext(filename)) {
	case ".pdf":
		return "application/pdf"
	case ".txt", ".md", ".csv", ".json", ".log", ".xml", ".html":
		return "text/plain"
	}
	if len(data) >= 8 {
		if head, err := base64.StdEncoding.DecodeString(data[:8]); err == nil && strings.HasPrefix(string(head), "%PDF") {
			return "application/pdf"
		}
	}
	return "application/octet-stream"
}
