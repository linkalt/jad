package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
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

// MCPResourceText is a resource read for the draft: its text and its images and PDFs.
type MCPResourceText struct {
	Server      string                  `json:"server"`
	URI         string                  `json:"uri"`
	MIMEType    string                  `json:"mime_type,omitempty"`
	Text        string                  `json:"text"`
	Attachments []MCPResourceAttachment `json:"attachments,omitempty"`
}

// MCPResourceAttachment is an image ("image") or PDF ("file", with Name) of a resource,
// base64 in Data, in the shape a run input part takes.
type MCPResourceAttachment struct {
	Type     string `json:"type"`
	URI      string `json:"uri"`
	MIMEType string `json:"mime_type"`
	Name     string `json:"name,omitempty"`
	Data     string `json:"data"`
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

// MCPResourceEvent is one change a server reported: kind "updated" (a subscribed resource,
// uri set) or "list_changed".
type MCPResourceEvent struct {
	Seq    int64     `json:"seq"`
	Server string    `json:"server"`
	Kind   string    `json:"kind"`
	URI    string    `json:"uri,omitempty"`
	At     time.Time `json:"at"`
}

// MCPResourceSubscription is one resource the profile's servers are watched for.
type MCPResourceSubscription struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

// MCPResourceEvents answers the events route: the events after the asked seq, the newest
// seq to ask after next time, and the subscriptions held now.
type MCPResourceEvents struct {
	Events        []MCPResourceEvent        `json:"events"`
	Latest        int64                     `json:"latest"`
	Subscriptions []MCPResourceSubscription `json:"subscriptions"`
}

// SessionMCPResourceWatch subscribes to resources and lists their change notifications. It is
// optional beside SessionMCPResources; without it the routes answer 501.
type SessionMCPResourceWatch interface {
	SubscribeMCPResource(ctx context.Context, reference, profile, server, uri string, on bool) error
	MCPResourceEvents(ctx context.Context, reference, profile string, after int64) (MCPResourceEvents, error)
}

func (h *Handler) resourceWatch(w http.ResponseWriter) (SessionMCPResourceWatch, bool) {
	watch, ok := h.mcpResources.(SessionMCPResourceWatch)
	if !ok || h.mcpResources == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "MCP resource subscriptions are unavailable"})
		return nil, false
	}
	return watch, true
}

// handleMCPResourceSubscribe takes {"server", "uri"} and subscribes (on) or unsubscribes.
func (h *Handler) handleMCPResourceSubscribe(w http.ResponseWriter, r *http.Request, reference, profile string, on bool) {
	watch, ok := h.resourceWatch(w)
	if !ok {
		return
	}
	fields, err := readInteractionBody(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	for key := range fields {
		if key != "server" && key != "uri" {
			writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "unknown field " + key + "; the body takes server and uri"})
			return
		}
	}
	var server, uri string
	if json.Unmarshal(fields["server"], &server) != nil || strings.TrimSpace(server) == "" {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "server must be a non-empty string"})
		return
	}
	if json.Unmarshal(fields["uri"], &uri) != nil || strings.TrimSpace(uri) == "" {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "uri must be a non-empty string"})
		return
	}
	if err := watch.SubscribeMCPResource(r.Context(), reference, profile, server, uri, on); err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": server, "uri": uri, "subscribed": on})
}

// handleMCPResourceEvents lists the change notifications after ?after=<seq> (0 by default).
func (h *Handler) handleMCPResourceEvents(w http.ResponseWriter, r *http.Request, reference, profile string) {
	watch, ok := h.resourceWatch(w)
	if !ok {
		return
	}
	var after int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "after must be a non-negative integer"})
			return
		}
		after = parsed
	}
	events, err := watch.MCPResourceEvents(r.Context(), reference, profile, after)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	if events.Events == nil {
		events.Events = []MCPResourceEvent{}
	}
	if events.Subscriptions == nil {
		events.Subscriptions = []MCPResourceSubscription{}
	}
	writeJSON(w, http.StatusOK, events)
}
