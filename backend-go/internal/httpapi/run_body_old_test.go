package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The functions in this file are the run-body parser as it was before the one-pass change
// (copied from git), so the tests can hold the new parser to the same results and errors.

func oldParseRunInputSized(raw json.RawMessage, limits RunInputLimits) (json.RawMessage, int64, error) {
	imageChars := int64(0)
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
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
		fields, part := map[string]json.RawMessage{}, runInputPart{}
		if json.Unmarshal(rawPart, &fields) != nil || json.Unmarshal(rawPart, &part) != nil {
			return nil, 0, badInput(label + " must be an object")
		}
		switch part.Type {
		case "text":
			if err := oldOnlyFields(label, fields, "type", "text"); err != nil {
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
			if err := oldOnlyFields(label, fields, "type", "data"); err != nil {
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
			if err := oldOnlyFields(label, fields, "type", "name", "data"); err != nil {
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

func oldOnlyFields(label string, fields map[string]json.RawMessage, allowed ...string) error {
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

func oldReadRunBodyFromRaw(raw []byte, baseBytes int64, limits RunInputLimits) (runRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		// Unmarshal has already scanned the whole body; the checks below only pick the message.
		if !json.Valid(raw) {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			var first json.RawMessage
			if decoder.Decode(&first) != nil {
				return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "invalid JSON body"}
			}
			return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "request body must contain one JSON object"}
		}
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "input must be a non-empty string"}
	}

	inputRaw, exists := fields["input"]
	var body runRequest
	if !exists {
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "input must be a non-empty string"}
	}
	input, imageChars, err := oldParseRunInputSized(inputRaw, limits)
	if err != nil {
		return runRequest{}, err
	}
	// The reader allowed the image data on top of max_body_bytes; everything else in the
	// body still has to fit in max_body_bytes itself.
	if int64(len(raw))-imageChars > baseBytes {
		return runRequest{}, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
	}
	body.Input = input
	if modelRaw, exists := fields["model"]; exists {
		if json.Unmarshal(modelRaw, &body.Model) != nil || strings.TrimSpace(body.Model) == "" {
			return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "model must be a non-empty string"}
		}
	}
	if supportsRaw, exists := fields["supports_interaction"]; exists {
		_ = json.Unmarshal(supportsRaw, &body.SupportsInteraction)
	}
	body.Model = strings.TrimSpace(body.Model)
	return body, nil
}
