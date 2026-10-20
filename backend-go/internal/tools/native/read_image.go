package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"jad/backend-go/internal/imagedata"
	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const readImageToolID = "native.read_image"

var readImageInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","minLength":1,"description":"Image file (png, jpeg, webp or gif), relative to cwd or absolute."}
  },
  "required":["path"],
  "additionalProperties":false
}`)

type readImageResultPart struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
}

func normalizeReadImageArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	if path, ok := arguments["path"].(string); ok {
		normalized["path"] = strings.TrimSpace(path)
	}
	return normalized
}

// NewReadImageTool constructs native.read_image: the model sees the image itself, as one
// input_image part. The store is the one stored-image references are opened from.
func NewReadImageTool(settings sessionfs.NativeReadImageSettings, store *imagestore.Store) (tools.Tool, error) {
	validator, err := tools.CompileSchema(readImageInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native read_image schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          readImageToolID,
			Source:      "native",
			Name:        "read_image",
			Description: "Look at an image file (png, jpeg, webp or gif); use read for text.",
			InputSchema: append(json.RawMessage(nil), readImageInputSchema...),
		},
		Validator:          validator,
		ReadOnly:           true,
		ExampleOperation:   json.RawMessage(`{"path":"screenshot.png"}`),
		NormalizeArguments: normalizeReadImageArguments,
		ExecuteOutput: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (json.RawMessage, error) {
			return executeNativeReadImage(arguments, settings, store, execution)
		},
	}, nil
}

func executeNativeReadImage(arguments map[string]any, settings sessionfs.NativeReadImageSettings, store *imagestore.Store, execution tools.ExecutionContext) (json.RawMessage, error) {
	path, _ := arguments["path"].(string)
	data, failure := readImageFile(path, settings.Policy, store, settings.MaxImageBytes, execution, "")
	if failure != "" {
		return json.Marshal(failure)
	}
	mime, err := imagedata.Sniff(data)
	if err != nil {
		return json.Marshal(tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "unsupported_image", "The file is "+mime+"; png, jpeg, webp and gif are supported", tools.Information{Key: "path", Value: path}))
	}
	return json.Marshal([]readImageResultPart{{Type: "input_image", ImageURL: imagedata.DataURL(mime, data), Detail: "auto"}})
}
