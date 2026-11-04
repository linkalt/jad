package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// MCPPromptArgument is one argument an MCP prompt takes.
type MCPPromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// MCPPrompt is one prompt template of an MCP server the profile's toolsets turn prompts on for.
type MCPPrompt struct {
	Server      string              `json:"server"`
	Name        string              `json:"name"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Arguments   []MCPPromptArgument `json:"arguments"`
}

// MCPPromptFailure is a server whose prompts could not be listed.
type MCPPromptFailure struct {
	Server string `json:"server"`
	Error  string `json:"error"`
}

// MCPPromptList is the answer of the mcp-prompts route.
type MCPPromptList struct {
	Prompts  []MCPPrompt        `json:"prompts"`
	Failures []MCPPromptFailure `json:"failures,omitempty"`
}

// MCPPromptRequest asks for one prompt filled with its arguments.
type MCPPromptRequest struct {
	Server    string
	Name      string
	Arguments map[string]string
}

// MCPPromptText is a prompt filled with its arguments, as text for the draft.
type MCPPromptText struct {
	Server      string `json:"server"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Text        string `json:"text"`
}

// SessionMCPPrompts lists and fills the MCP prompts of one session profile: the prompts of
// the servers its toolsets list with prompts: true. Filling a prompt changes nothing in the
// session; the text goes to the person's draft, not to the transcript.
type SessionMCPPrompts interface {
	ListMCPPrompts(ctx context.Context, reference, profile string) (MCPPromptList, error)
	GetMCPPrompt(ctx context.Context, reference, profile string, request MCPPromptRequest) (MCPPromptText, error)
}

func (h *Handler) handleMCPPromptList(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.mcpPrompts == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "MCP prompts are unavailable"})
		return
	}
	listed, err := h.mcpPrompts.ListMCPPrompts(r.Context(), reference, profile)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listed)
}

func (h *Handler) handleMCPPromptGet(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.mcpPrompts == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "MCP prompts are unavailable"})
		return
	}
	fields, err := readInteractionBody(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	request, err := parseMCPPromptRequest(fields)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	filled, err := h.mcpPrompts.GetMCPPrompt(r.Context(), reference, profile, request)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]MCPPromptText{"prompt": filled})
}

// parseMCPPromptRequest reads {"server", "name", "arguments"}; arguments is an object of
// strings and may be left out.
func parseMCPPromptRequest(fields map[string]json.RawMessage) (MCPPromptRequest, error) {
	unknown := make([]string, 0)
	for key := range fields {
		if key != "server" && key != "name" && key != "arguments" {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return MCPPromptRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "unknown fields: " + strings.Join(unknown, ", ") + "; the body takes server, name and arguments"}
	}
	var request MCPPromptRequest
	if json.Unmarshal(fields["server"], &request.Server) != nil || strings.TrimSpace(request.Server) == "" {
		return MCPPromptRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "server must be a non-empty string"}
	}
	if json.Unmarshal(fields["name"], &request.Name) != nil || strings.TrimSpace(request.Name) == "" {
		return MCPPromptRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "name must be a non-empty string"}
	}
	if raw, exists := fields["arguments"]; exists && string(raw) != "null" {
		if json.Unmarshal(raw, &request.Arguments) != nil {
			return MCPPromptRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "arguments must be an object of strings"}
		}
	}
	return request, nil
}
