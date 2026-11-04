package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// MCPResource is one resource, or resource template, of an MCP server the profile's
// toolsets turn resources on for. A resource has URI; a template has URITemplate and the
// Variables a person fills.
type MCPResource struct {
	Server      string   `json:"server"`
	URI         string   `json:"uri,omitempty"`
	URITemplate string   `json:"uri_template,omitempty"`
	Variables   []string `json:"variables,omitempty"`
	Name        string   `json:"name"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	MIMEType    string   `json:"mime_type,omitempty"`
}

// MCPResourceList is the answer of the mcp-resources route.
type MCPResourceList struct {
	Resources []MCPResource      `json:"resources"`
	Failures  []MCPPromptFailure `json:"failures,omitempty"`
}

// MCPResourceRequest names one resource: URI, or URITemplate filled with Variables.
type MCPResourceRequest struct {
	Server      string
	URI         string
	URITemplate string
	Variables   map[string]string
}

// MCPResourceText is a resource read as text for the draft.
type MCPResourceText struct {
	Server   string `json:"server"`
	URI      string `json:"uri"`
	MIMEType string `json:"mime_type,omitempty"`
	Text     string `json:"text"`
}

// SessionMCPResources lists and reads the MCP resources of one session profile: those of
// the servers its toolsets list with resources: true. Reading changes nothing in the
// session; the text goes to the person's draft.
type SessionMCPResources interface {
	ListMCPResources(ctx context.Context, reference, profile string) (MCPResourceList, error)
	ReadMCPResource(ctx context.Context, reference, profile string, request MCPResourceRequest) (MCPResourceText, error)
}

func (h *Handler) handleMCPResourceList(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.mcpResources == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "MCP resources are unavailable"})
		return
	}
	listed, err := h.mcpResources.ListMCPResources(r.Context(), reference, profile)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listed)
}

func (h *Handler) handleMCPResourceRead(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.mcpResources == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "MCP resources are unavailable"})
		return
	}
	fields, err := readInteractionBody(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	request, err := parseMCPResourceRequest(fields)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	read, err := h.mcpResources.ReadMCPResource(r.Context(), reference, profile, request)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]MCPResourceText{"resource": read})
}

// parseMCPResourceRequest reads {"server", "uri"} or {"server", "uri_template",
// "variables"}; variables is an object of strings and may be left out.
func parseMCPResourceRequest(fields map[string]json.RawMessage) (MCPResourceRequest, error) {
	unknown := make([]string, 0)
	for key := range fields {
		if key != "server" && key != "uri" && key != "uri_template" && key != "variables" {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return MCPResourceRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "unknown fields: " + strings.Join(unknown, ", ") + "; the body takes server, uri, uri_template and variables"}
	}
	var request MCPResourceRequest
	if json.Unmarshal(fields["server"], &request.Server) != nil || strings.TrimSpace(request.Server) == "" {
		return MCPResourceRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "server must be a non-empty string"}
	}
	text := func(name string, target *string) error {
		raw, exists := fields[name]
		if !exists {
			return nil
		}
		if json.Unmarshal(raw, target) != nil || strings.TrimSpace(*target) == "" {
			return &StatusError{Code: http.StatusBadRequest, Message: name + " must be a non-empty string"}
		}
		return nil
	}
	if err := text("uri", &request.URI); err != nil {
		return MCPResourceRequest{}, err
	}
	if err := text("uri_template", &request.URITemplate); err != nil {
		return MCPResourceRequest{}, err
	}
	if (request.URI == "") == (request.URITemplate == "") {
		return MCPResourceRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "give uri or uri_template, not both"}
	}
	if raw, exists := fields["variables"]; exists && string(raw) != "null" {
		if request.URI != "" {
			return MCPResourceRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "variables go with uri_template only"}
		}
		if json.Unmarshal(raw, &request.Variables) != nil {
			return MCPResourceRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "variables must be an object of strings"}
		}
	}
	return request, nil
}
