package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"example.com/agent-harness/backend-go/internal/imagedata"
)

// RunInputLimits bounds the images and files of one run request. The values come from the
// server.max_request_images, server.max_request_image_bytes, server.max_request_files and
// server.max_request_file_bytes settings.
type RunInputLimits struct {
	// MaxImages is the most images one request may carry.
	MaxImages int
	// MaxImageBytes is the largest decoded image, in bytes.
	MaxImageBytes int64
	// MaxFiles is the most files one request may carry.
	MaxFiles int
	// MaxFileBytes is the largest decoded file, in bytes.
	MaxFileBytes int64
}

// runInputPart is one element of an array input: {"type":"text","text":...},
// {"type":"image","data":...} or {"type":"file","name":...,"data":...}. Any other field is
// refused, so a client that sends a path or a URL learns at once that the server does not
// read it.
type runInputPart struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
	Data *string `json:"data"`
	Name *string `json:"name"`
}

// userMessageContent is one part of the stored user message.
type userMessageContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// Filename and FileData are an input_file part: a PDF as a base64 data URL.
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

// parseRunInput turns the "input" of a run request into what the runner takes: a JSON
// string for a text input (the shape it has always had, so the transcript is the same),
// or one Responses user message for an array of parts. A bad request is a StatusError
// with code 400. The images are decoded only to check them: size, base64 and type by
// the first bytes. The message keeps them as data URLs, and the image store, when it is
// on, moves them out of the transcript afterwards.
func parseRunInput(raw json.RawMessage, limits RunInputLimits) (json.RawMessage, error) {
	input, _, err := parseRunInputSized(raw, limits)
	return input, err
}

// parseRunInputSized is parseRunInput that also reports how many bytes of the request
// are image and file data (the length of every "data" string). The run body limit counts
// the rest of the body against server.max_body_bytes, and only that data against the
// image and file allowance.
func parseRunInputSized(raw json.RawMessage, limits RunInputLimits) (json.RawMessage, int64, error) {
	imageChars := int64(0)
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "[") {
		var text string
		if json.Unmarshal(raw, &text) != nil || strings.TrimSpace(text) == "" {
			return nil, 0, badInput("input must be a non-empty string")
		}
		encoded, err := json.Marshal(text)
		return encoded, 0, err
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return nil, 0, badInput("input must be a non-empty string or a non-empty array of parts")
	}
	content := make([]userMessageContent, 0, len(parts))
	images, files, hasText := 0, 0, false
	for index, rawPart := range parts {
		label := fmt.Sprintf("input part %d", index+1)
		fields, part := map[string]json.RawMessage{}, runInputPart{}
		if json.Unmarshal(rawPart, &fields) != nil || json.Unmarshal(rawPart, &part) != nil {
			return nil, 0, badInput(label + " must be an object")
		}
		switch part.Type {
		case "text":
			if err := onlyFields(label, fields, "type", "text"); err != nil {
				return nil, 0, err
			}
			if part.Text == nil {
				return nil, 0, badInput(label + " needs a text string")
			}
			if strings.TrimSpace(*part.Text) == "" {
				continue
			}
			hasText = true
			content = append(content, userMessageContent{Type: "input_text", Text: *part.Text})
		case "image":
			if err := onlyFields(label, fields, "type", "data"); err != nil {
				return nil, 0, err
			}
			if part.Data == nil {
				return nil, 0, badInput(label + " needs data (base64 or a data URL)")
			}
			images++
			imageChars += int64(len(*part.Data))
			if limits.MaxImages <= 0 {
				return nil, 0, badInput("image input is off (server.max_request_images is 0)")
			}
			if images > limits.MaxImages {
				return nil, 0, badInput(fmt.Sprintf("input has more than %d images", limits.MaxImages))
			}
			url, err := checkedImageURL(label, *part.Data, limits.MaxImageBytes)
			if err != nil {
				return nil, 0, err
			}
			content = append(content, userMessageContent{Type: "input_image", ImageURL: url, Detail: "auto"})
		case "file":
			if err := onlyFields(label, fields, "type", "name", "data"); err != nil {
				return nil, 0, err
			}
			if part.Data == nil {
				return nil, 0, badInput(label + " needs data (base64 or a data URL)")
			}
			if part.Name == nil {
				return nil, 0, badInput(label + " needs a name")
			}
			files++
			imageChars += int64(len(*part.Data))
			if limits.MaxFiles <= 0 {
				return nil, 0, badInput("file input is off (server.max_request_files is 0)")
			}
			if files > limits.MaxFiles {
				return nil, 0, badInput(fmt.Sprintf("input has more than %d files", limits.MaxFiles))
			}
			name, err := cleanFilename(label, *part.Name)
			if err != nil {
				return nil, 0, err
			}
			fileData, err := checkedFileData(label, *part.Data, limits.MaxFileBytes)
			if err != nil {
				return nil, 0, err
			}
			content = append(content, userMessageContent{Type: "input_file", Filename: name, FileData: fileData})
		default:
			return nil, 0, badInput(label + " has an unknown type; use text, image or file")
		}
	}
	if !hasText && images == 0 && files == 0 {
		return nil, 0, badInput("input must have some text, an image or a file")
	}
	encoded, err := json.Marshal(struct {
		Type    string               `json:"type"`
		Role    string               `json:"role"`
		Content []userMessageContent `json:"content"`
	}{Type: "message", Role: "user", Content: content})
	return encoded, imageChars, err
}

// checkedImageURL decodes one image to check it and returns it as a data URL of the type
// its bytes show, whatever type the client declared.
func checkedImageURL(label, data string, maxBytes int64) (string, error) {
	decoded, err := imagedata.Decode(data, maxBytes)
	switch err {
	case nil:
	case imagedata.ErrTooLarge:
		return "", &StatusError{Code: 413, Message: fmt.Sprintf("%s: the image is larger than %d bytes", label, maxBytes)}
	default:
		return "", badInput(label + ": " + err.Error())
	}
	mime, err := imagedata.Sniff(decoded)
	if err != nil {
		return "", badInput(label + ": the image is " + mime + "; png, jpeg, webp and gif are supported")
	}
	return imagedata.DataURL(mime, decoded), nil
}

func onlyFields(label string, fields map[string]json.RawMessage, allowed ...string) error {
	for name := range fields {
		known := false
		for _, candidate := range allowed {
			known = known || name == candidate
		}
		if !known {
			return badInput(fmt.Sprintf("%s has an unknown field %q", label, name))
		}
	}
	return nil
}

func badInput(message string) error {
	return &StatusError{Code: 400, Message: message}
}
