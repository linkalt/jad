package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
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
		errorJSON, encodeErr := marshalJSON(struct {
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
	if len(structuredContent) > 0 {
		ordered, err := preserveStructuredContentOrder(structuredContent)
		if err != nil {
			return nil, fmt.Errorf("preserve MCP structured-content order: %w", err)
		}
		if result != nil {
			result.StructuredContent = ordered
		}
	}
	return marshalParts(boundedContent(result, current.maxResultBytes))
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
			if part.Type == "input_text" && len(bounded) == 0 {
				room := max(0, (maxBytes-used-noticeBytes-128)/6)
				text := truncateUTF8(*part.Text, room)
				if text != "" {
					truncated := textPart(text)
					bounded = append(bounded, truncated)
					used = partSize(truncated)
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
	encoded, err := marshalJSON(parts)
	if err != nil {
		return nil, fmt.Errorf("encode MCP tool result: %w", err)
	}
	return encoded, nil
}

func partSize(part outputPart) int {
	encoded, err := marshalJSON(part)
	if err != nil {
		return 0
	}
	return len(encoded)
}

func textPart(text string) outputPart {
	return outputPart{Type: "input_text", Text: &text}
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

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	bytes := []byte(value[:maxBytes])
	for len(bytes) > 0 && !utf8.Valid(bytes) {
		bytes = bytes[:len(bytes)-1]
	}
	return string(bytes)
}

func compactJSON(value any) (string, error) {
	encoded, err := marshalJSON(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func marshalJSON(value any) (json.RawMessage, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	return json.RawMessage(unescapeJSONSeparators(encoded)), nil
}

func unescapeJSONSeparators(encoded []byte) []byte {
	var buffer bytes.Buffer
	for index := 0; index < len(encoded); {
		if encoded[index] == '\\' && len(encoded)-index >= 6 {
			switch string(encoded[index : index+6]) {
			case `\u2028`:
				buffer.WriteRune('\u2028')
				index += 6
				continue
			case `\u2029`:
				buffer.WriteRune('\u2029')
				index += 6
				continue
			}
		}
		if encoded[index] == '\\' && index+1 < len(encoded) {
			buffer.Write(encoded[index : index+2])
			index += 2
			continue
		}
		buffer.WriteByte(encoded[index])
		index++
	}
	return buffer.Bytes()
}
