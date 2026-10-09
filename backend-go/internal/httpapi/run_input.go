package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"example.com/agent-harness/backend-go/internal/imagedata"
)

// RunInputLimits bounds the images of one run request. Both values come from the
// server.max_request_images and server.max_request_image_bytes settings.
type RunInputLimits struct {
	// MaxImages is the most images one request may carry.
	MaxImages int
	// MaxImageBytes is the largest decoded image, in bytes.
	MaxImageBytes int64
}

// runInputPart is one element of an array input: {"type":"text","text":...} or
// {"type":"image","data":...}. Any other field is refused, so a client that sends a
// path or a URL learns at once that the server does not read it.
type runInputPart struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
	Data *string `json:"data"`
}

// userMessageContent is one part of the stored user message.
type userMessageContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
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
// are image data (the length of every image "data" string). The run body limit counts
// the rest of the body against server.max_body_bytes, and only the image data against the
// image allowance.
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
	images, hasText := 0, false
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
		default:
			return nil, 0, badInput(label + " has an unknown type; use text or image")
		}
	}
	if !hasText && images == 0 {
		return nil, 0, badInput("input must have some text or an image")
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
