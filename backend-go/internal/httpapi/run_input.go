package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"jad/backend-go/internal/imagedata"
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
	if !json.Valid(raw) {
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			return nil, 0, badInput("input must be a non-empty string or a non-empty array of parts")
		}
		return nil, 0, badInput("input must be a non-empty string")
	}
	return parseValidRunInput(raw, limits)
}

// parseValidRunInput is parseRunInputSized for raw that json.Valid has accepted. It reads the
// parts in place, so the request is not copied into a RawMessage and two maps per part.
func parseValidRunInput(raw []byte, limits RunInputLimits) (json.RawMessage, int64, error) {
	imageChars := int64(0)
	if firstJSONByte(raw) != '[' {
		var text string
		if json.Unmarshal(raw, &text) != nil || strings.TrimSpace(text) == "" {
			return nil, 0, badInput("input must be a non-empty string")
		}
		encoded, err := json.Marshal(text)
		return encoded, 0, err
	}
	parts := jsonArrayElements(raw)
	if len(parts) == 0 {
		return nil, 0, badInput("input must be a non-empty string or a non-empty array of parts")
	}
	// The message is encoded straight into one buffer, sized from the request's own data, so a
	// large image or file is never held as a decoded copy, a data URL string and a marshalled copy.
	size := 128
	for _, rawPart := range parts {
		size += len(rawPart) + 128
	}
	out := append(make([]byte, 0, size), `{"type":"message","role":"user","content":[`...)
	written := 0
	images, files, hasText := 0, 0, false
	for index, rawPart := range parts {
		label := fmt.Sprintf("input part %d", index+1)
		part, fields, ok := decodeRunInputPart(rawPart)
		if !ok {
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
			out = appendPartSeparator(out, &written)
			out = append(out, `{"type":"input_text","text":`...)
			out = appendJSONString(out, *part.Text)
			out = append(out, '}')
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
			mime, decoded, err := checkedImage(label, *part.Data, limits.MaxImageBytes)
			if err != nil {
				return nil, 0, err
			}
			out = appendPartSeparator(out, &written)
			out = append(out, `{"type":"input_image","image_url":"data:`...)
			out = append(out, mime...)
			out = append(out, ";base64,"...)
			out = base64.StdEncoding.AppendEncode(out, decoded)
			out = append(out, `","detail":"auto"}`...)
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
			decoded, err := checkedFile(label, *part.Data, limits.MaxFileBytes)
			if err != nil {
				return nil, 0, err
			}
			out = appendPartSeparator(out, &written)
			out = append(out, `{"type":"input_file","filename":`...)
			out = appendJSONString(out, name)
			out = append(out, `,"file_data":"data:application/pdf;base64,`...)
			out = base64.StdEncoding.AppendEncode(out, decoded)
			out = append(out, `"}`...)
		default:
			return nil, 0, badInput(label + " has an unknown type; use text, image or file")
		}
	}
	if !hasText && images == 0 && files == 0 {
		return nil, 0, badInput("input must have some text, an image or a file")
	}
	return append(out, "]}"...), imageChars, nil
}

func appendPartSeparator(out []byte, written *int) []byte {
	if *written > 0 {
		out = append(out, ',')
	}
	*written++
	return out
}

// appendJSONString appends text as encoding/json writes a string field (HTML characters escaped).
func appendJSONString(out []byte, text string) []byte {
	encoded, _ := json.Marshal(text)
	return append(out, encoded...)
}

// checkedImage decodes one image to check it and returns the type its bytes show, whatever
// type the client declared, and the decoded bytes.
func checkedImage(label, data string, maxBytes int64) (string, []byte, error) {
	decoded, err := imagedata.Decode(data, maxBytes)
	switch err {
	case nil:
	case imagedata.ErrTooLarge:
		return "", nil, &StatusError{Code: 413, Message: fmt.Sprintf("%s: the image is larger than %d bytes", label, maxBytes)}
	default:
		return "", nil, badInput(label + ": " + err.Error())
	}
	mime, err := imagedata.Sniff(decoded)
	if err != nil {
		return "", nil, badInput(label + ": the image is " + mime + "; png, jpeg, webp and gif are supported")
	}
	return mime, decoded, nil
}

func onlyFields(label string, fields []string, allowed ...string) error {
	for _, name := range fields {
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
