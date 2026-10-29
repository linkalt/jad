package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/tools"
)

var mcpMIMETypePattern = regexp.MustCompile(`^[a-zA-Z0-9!#$&^_.+-]+/[a-zA-Z0-9!#$&^_.+-]+$`)

type outputPart struct {
	Type     string  `json:"type"`
	Text     *string `json:"text,omitempty"`
	ImageURL *string `json:"image_url,omitempty"`
	Detail   string  `json:"detail,omitempty"`
	FileData *string `json:"file_data,omitempty"`
	Filename string  `json:"filename,omitempty"`
}

func executeTool(ctx context.Context, current *connection, serverID, remoteName string, arguments map[string]any) (json.RawMessage, error) {
	callCtx, cancel := context.WithTimeout(ctx, current.timeout)
	defer cancel()
	result, structuredContent, err := current.callTool(callCtx, &mcpSDK.CallToolParams{Name: remoteName, Arguments: arguments})
	if err != nil {
		errorJSON, encodeErr := tools.MarshalJSON(struct {
			Status string `json:"status"`
			Server string `json:"server"`
			Tool   string `json:"tool"`
			Error  string `json:"error"`
		}{Status: "error", Server: serverID, Tool: remoteName, Error: safeErrorType(err)})
		if encodeErr != nil {
			return nil, fmt.Errorf("encode MCP tool error result: %w", encodeErr)
		}
		return marshalParts([]outputPart{textPart(string(errorJSON))})
	}
	if len(structuredContent) > 0 && result != nil {
		content, err := structuredContentForResult(structuredContent, current.maxResultBytes)
		if err != nil {
			return nil, err
		}
		result.StructuredContent = content
	}
	return marshalParts(boundedContent(result, current.maxResultBytes))
}

// structuredContentForResult converts captured structured content into the value of the
// tool result. A payload larger than maxResultBytes is kept as the server sent it: the
// content parts are cut to that limit below, so ordering it first would only spend work
// on bytes that never reach the model. Keeping the raw value also bounds the conversion
// itself, because a JSON value of n bytes nests at most n/2 levels, and n is at most
// max_result_bytes here; the conversion therefore needs no nesting limit of its own.
func structuredContentForResult(raw json.RawMessage, maxResultBytes int) (any, error) {
	if len(raw) > maxResultBytes {
		return raw, nil
	}
	ordered, err := preserveStructuredContentOrder(raw)
	if err != nil {
		return nil, fmt.Errorf("preserve MCP structured-content order: %w", err)
	}
	return ordered, nil
}

func contentParts(result *mcpSDK.CallToolResult) []outputPart {
	if result == nil {
		return []outputPart{textPart("MCP server returned an invalid tool result.")}
	}
	parts := make([]outputPart, 0)
	if result.IsError {
		parts = append(parts, textPart("MCP server reported a tool error."))
	}
	for _, block := range result.Content {
		switch content := block.(type) {
		case *mcpSDK.TextContent:
			if content != nil {
				parts = append(parts, textPart(content.Text))
			}
		case *mcpSDK.ImageContent:
			if content != nil && validImage(content.MIMEType, content.Data) {
				parts = append(parts, imagePart(content.MIMEType, content.Data))
			} else {
				parts = append(parts, textPart("MCP image content was omitted because its MIME type or base64 data was invalid."))
			}
		case *mcpSDK.EmbeddedResource:
			parts = append(parts, embeddedResourceParts(content)...)
		case *mcpSDK.ResourceLink:
			if content == nil {
				continue
			}
			label := content.Name
			if label == "" {
				label = "MCP resource"
			}
			mime := ""
			if content.MIMEType != "" {
				mime = " (" + content.MIMEType + ")"
			}
			uri := ""
			if content.URI != "" {
				uri = ": " + content.URI
			}
			parts = append(parts, textPart(label+mime+uri))
		case *mcpSDK.AudioContent:
			mime := "unknown"
			if content != nil && content.MIMEType != "" {
				mime = content.MIMEType
			}
			parts = append(parts, textPart(fmt.Sprintf("MCP audio content omitted (MIME type %s); audio input is not enabled.", mime)))
		default:
			parts = append(parts, textPart("Unsupported MCP content block: unknown."))
		}
	}
	if result.StructuredContent != nil {
		serialized, err := compactJSON(result.StructuredContent)
		if err != nil {
			serialized = "[unserializable structured content]"
		}
		parts = append(parts, textPart("Structured content:\n"+serialized))
	}
	if len(parts) == 0 {
		return []outputPart{textPart("MCP tool returned no content.")}
	}
	return parts
}

func embeddedResourceParts(content *mcpSDK.EmbeddedResource) []outputPart {
	if content == nil || content.Resource == nil {
		return []outputPart{textPart("MCP embedded resource had no supported text or binary content.")}
	}
	resource := content.Resource
	if resource.Text != "" {
		prefix := ""
		if resource.URI != "" {
			prefix = "Resource " + resource.URI + ":\n"
		}
		return []outputPart{textPart(prefix + resource.Text)}
	}
	if len(resource.Blob) > 0 && validMIMEType(resource.MIMEType) {
		if strings.HasPrefix(resource.MIMEType, "image/") {
			return []outputPart{imagePart(resource.MIMEType, resource.Blob)}
		}
		if strings.HasPrefix(resource.MIMEType, "audio/") {
			return []outputPart{textPart(fmt.Sprintf("MCP audio resource omitted (MIME type %s); audio input is not enabled.", resource.MIMEType))}
		}
		return []outputPart{filePart(resource.URI, resource.Blob)}
	}
	return []outputPart{textPart("MCP embedded resource had no supported text or binary content.")}
}

func boundedContent(result *mcpSDK.CallToolResult, maxBytes int) []outputPart {
	parts := contentParts(result)
	bounded := make([]outputPart, 0, len(parts))
	used := 0
	const notice = "[Additional MCP result content omitted at the configured max_result_bytes limit.]"
	noticePart := textPart(notice)
	noticeBytes := partSize(noticePart)
	for _, part := range parts {
		size := partSize(part)
		if used+size+noticeBytes > maxBytes {
			// The part that does not fit is cut rather than dropped, wherever it sits in
			// the result: an isError message or a single large text then still reaches the
			// model instead of vanishing behind the omission notice.
			if part.Type == "input_text" {
				if truncated, truncatedSize, ok := fitTextPart(*part.Text, maxBytes-used-noticeBytes); ok {
					bounded = append(bounded, truncated)
					used += truncatedSize
				}
			}
			if used+noticeBytes <= maxBytes {
				bounded = append(bounded, noticePart)
			}
			break
		}
		bounded = append(bounded, part)
		used += size
	}
	if len(bounded) > 0 {
		return bounded
	}
	return []outputPart{noticePart}
}

func marshalParts(parts []outputPart) (json.RawMessage, error) {
	if parts == nil {
		parts = []outputPart{}
	}
	encoded, err := tools.MarshalJSON(parts)
	if err != nil {
		return nil, fmt.Errorf("encode MCP tool result: %w", err)
	}
	return encoded, nil
}

func partSize(part outputPart) int {
	encoded, err := tools.MarshalJSON(part)
	if err != nil {
		return 0
	}
	return len(encoded)
}

func textPart(text string) outputPart {
	return outputPart{Type: "input_text", Text: &text}
}

// fitTextPart cuts text to the longest prefix whose encoded part still fits in budget
// bytes, and reports that encoded size. It measures the encoding instead of assuming a
// worst-case escape expansion, which used to leave most of the budget unspent.
func fitTextPart(text string, budget int) (outputPart, int, bool) {
	room := min(len(text), budget-partSize(textPart("")))
	for room > 0 {
		cut, _ := tools.CutText(text, int64(room))
		part := textPart(cut)
		size := partSize(part)
		if size <= budget {
			if cut == "" {
				return outputPart{}, 0, false
			}
			return part, size, true
		}
		// Escaping made the part too large: shrink by the measured ratio, at least one byte,
		// so the loop always makes progress.
		next := room * budget / size
		if next >= room {
			next = room - 1
		}
		room = next
	}
	return outputPart{}, 0, false
}

func imagePart(mimeType string, data []byte) outputPart {
	imageURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
	return outputPart{Type: "input_image", ImageURL: &imageURL, Detail: "auto"}
}

func filePart(uri string, data []byte) outputPart {
	encoded := base64.StdEncoding.EncodeToString(data)
	return outputPart{Type: "input_file", FileData: &encoded, Filename: resourceFilename(uri)}
}

func validImage(mimeType string, data []byte) bool {
	return validMIMEType(mimeType) && strings.HasPrefix(mimeType, "image/") && len(data) > 0
}

func validMIMEType(value string) bool {
	return mcpMIMETypePattern.MatchString(value)
}

func resourceFilename(uri string) string {
	if uri == "" {
		return "mcp-resource.bin"
	}
	parsed, err := url.Parse(uri)
	pathValue := uri
	if err == nil {
		pathValue = parsed.EscapedPath()
	}
	name := path.Base(strings.SplitN(strings.SplitN(pathValue, "?", 2)[0], "#", 2)[0])
	var builder strings.Builder
	for _, character := range name {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 || builder.String() == "." || builder.String() == ".." {
		return "mcp-resource.bin"
	}
	return builder.String()
}

func compactJSON(value any) (string, error) {
	encoded, err := tools.MarshalJSON(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
