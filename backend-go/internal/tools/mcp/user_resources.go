package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yosida95/uritemplate/v3"

	"jad/backend-go/internal/sessionfs"
)

// The slash menu's view of MCP resources. A toolset entry with resources: true offers its
// server's resources to the model as tools (featureTools) and here to the person, who
// picks one and gets its text in the draft. A template (resources/templates/list) is
// listed with the variables of its URI template; the person fills them before it is read.

// UserResource is one resource, or one resource template, a server offers. A resource has
// URI; a template has URITemplate and Variables instead.
type UserResource struct {
	Server      string   `json:"server"`
	URI         string   `json:"uri,omitempty"`
	URITemplate string   `json:"uri_template,omitempty"`
	Variables   []string `json:"variables,omitempty"`
	Name        string   `json:"name"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	MIMEType    string   `json:"mime_type,omitempty"`
}

// UserResourceList is every resource and template of the servers with resources: true, in
// toolset order, each server's resources before its templates.
type UserResourceList struct {
	Resources []UserResource      `json:"resources"`
	Failures  []UserPromptFailure `json:"failures,omitempty"`
}

// UserResourceText is a resource read as text for the draft.
type UserResourceText struct {
	Server   string `json:"server"`
	URI      string `json:"uri"`
	MIMEType string `json:"mime_type,omitempty"`
	Text     string `json:"text"`
}

// UserResourceRead names the resource to read: URI, or URITemplate filled with Variables.
type UserResourceRead struct {
	Server      string
	URI         string
	URITemplate string
	Variables   map[string]string
}

func resourceServers(servers []sessionfs.MCPServerReference) []sessionfs.MCPServerReference {
	selected := make([]sessionfs.MCPServerReference, 0, len(servers))
	for _, server := range servers {
		if server.Enabled && server.Resources {
			selected = append(selected, server)
		}
	}
	return selected
}

// ListUserResources lists the resources and templates of every server with resources:
// true, asking the servers at the same time. A server that fails is in Failures and the
// rest are listed. Each server's list (resources and templates together) stops at its
// max_tools.
func (m *Manager) ListUserResources(ctx context.Context, servers []sessionfs.MCPServerReference, cwd string) (UserResourceList, error) {
	if ctx == nil {
		return UserResourceList{}, errors.New("MCP resource listing context is required")
	}
	selected := resourceServers(servers)
	type serverResult struct {
		resources []UserResource
		err       string
	}
	results := make([]serverResult, len(selected))
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxConcurrentServerLoads)
	for index, server := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			resources, err := m.listServerResources(ctx, server, cwd)
			if err != nil {
				results[index].err = err.Error()
				return
			}
			results[index].resources = resources
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return UserResourceList{}, err
	}
	list := UserResourceList{Resources: []UserResource{}}
	for index, result := range results {
		if result.err != "" {
			list.Failures = append(list.Failures, UserPromptFailure{Server: selected[index].ID, Error: result.err})
			continue
		}
		list.Resources = append(list.Resources, result.resources...)
	}
	return list, nil
}

func (m *Manager) listServerResources(ctx context.Context, server sessionfs.MCPServerReference, cwd string) ([]UserResource, error) {
	current, err := m.resourceConnection(ctx, server, cwd)
	if err != nil {
		return nil, err
	}
	resources := []UserResource{}
	add := func(entry UserResource) error {
		if len(resources) == current.maxTools {
			return &listingError{fmt.Sprintf("the server lists more than max_tools (%d) resources and templates", current.maxTools)}
		}
		entry.Server = server.ID
		entry.Title = cutText(entry.Title, current.maxDescriptionBytes)
		entry.Description = cutText(entry.Description, current.maxDescriptionBytes)
		resources = append(resources, entry)
		return nil
	}
	if err := pages(ctx, current, "resources/list", func(ctx context.Context, session *mcpSDK.ClientSession, cursor string) (string, error) {
		result, err := session.ListResources(ctx, &mcpSDK.ListResourcesParams{Cursor: cursor})
		if err != nil || result == nil {
			return "", err
		}
		for _, resource := range result.Resources {
			if resource == nil || strings.TrimSpace(resource.URI) == "" {
				continue
			}
			if err := add(UserResource{URI: resource.URI, Name: resource.Name, Title: resource.Title, Description: resource.Description, MIMEType: resource.MIMEType}); err != nil {
				return "", err
			}
		}
		return result.NextCursor, nil
	}); err != nil {
		return nil, err
	}
	if err := pages(ctx, current, "resources/templates/list", func(ctx context.Context, session *mcpSDK.ClientSession, cursor string) (string, error) {
		result, err := session.ListResourceTemplates(ctx, &mcpSDK.ListResourceTemplatesParams{Cursor: cursor})
		if err != nil || result == nil {
			return "", err
		}
		for _, template := range result.ResourceTemplates {
			if template == nil {
				continue
			}
			parsed, err := uritemplate.New(template.URITemplate)
			if err != nil || strings.TrimSpace(template.URITemplate) == "" {
				// A template that is not RFC 6570 cannot be filled; it is left out.
				continue
			}
			variables := uniqueNames(parsed.Varnames())
			if err := add(UserResource{URITemplate: template.URITemplate, Variables: variables, Name: template.Name, Title: template.Title, Description: template.Description, MIMEType: template.MIMEType}); err != nil {
				return "", err
			}
		}
		return result.NextCursor, nil
	}); err != nil {
		return nil, err
	}
	return resources, nil
}

// pages calls one list method page by page until it gives no cursor. A server that does
// not have the method (JSON-RPC method not found) lists nothing: a server may offer
// resources without templates.
func pages(ctx context.Context, current *connection, method string, page func(context.Context, *mcpSDK.ClientSession, string) (string, error)) error {
	cursor := ""
	seen := map[string]bool{}
	for {
		var next string
		var listing *listingError
		err := current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
			var err error
			next, err = page(ctx, session, cursor)
			if errors.As(err, &listing) {
				// The listing's own limit, not the server's failure.
				return nil
			}
			return err
		})
		var rpcError *jsonrpc.Error
		switch {
		case listing != nil:
			return errors.New(listing.message)
		case errors.As(err, &rpcError) && rpcError.Code == jsonrpc.CodeMethodNotFound:
			return nil
		case err != nil:
			return errors.New(userRequestError(err, current))
		case next == "":
			return nil
		case seen[next]:
			return fmt.Errorf("the server repeated a %s cursor", method)
		}
		seen[next] = true
		cursor = next
	}
}

// listingError is a limit the listing itself hit, as opposed to a request that failed.
type listingError struct{ message string }

func (e *listingError) Error() string { return e.message }

func uniqueNames(names []string) []string {
	seen := map[string]bool{}
	unique := make([]string, 0, len(names))
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			unique = append(unique, name)
		}
	}
	return unique
}

// ReadUserResource reads one resource of a server with resources: true as text: the text
// contents joined by blank lines, and a note for each binary one. A resource with no text
// at all, or with more text than the server's max_result_bytes, is refused.
func (m *Manager) ReadUserResource(ctx context.Context, servers []sessionfs.MCPServerReference, cwd string, request UserResourceRead) (UserResourceText, error) {
	if ctx == nil {
		return UserResourceText{}, errors.New("MCP resource context is required")
	}
	var server *sessionfs.MCPServerReference
	for _, candidate := range resourceServers(servers) {
		if candidate.ID == request.Server {
			server = &candidate
			break
		}
	}
	if server == nil {
		return UserResourceText{}, &UserPromptError{NotFound: true, Message: fmt.Sprintf("no MCP server %q with resources: true in the profile's toolsets", request.Server)}
	}
	uri, err := resourceURI(request)
	if err != nil {
		return UserResourceText{}, &UserPromptError{Invalid: true, Message: err.Error()}
	}
	current, err := m.resourceConnection(ctx, *server, cwd)
	if err != nil {
		return UserResourceText{}, &UserPromptError{Message: err.Error()}
	}
	var contents []*mcpSDK.ResourceContents
	err = current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
		result, err := session.ReadResource(ctx, &mcpSDK.ReadResourceParams{URI: uri})
		if result != nil {
			contents = result.Contents
		}
		return err
	})
	if err != nil {
		return UserResourceText{}, &UserPromptError{Message: fmt.Sprintf("MCP server %s: resource %s: %s", server.ID, uri, userRequestError(err, current))}
	}
	parts := make([]string, 0, len(contents))
	hasText, mimeType := false, ""
	for _, content := range contents {
		if content == nil {
			continue
		}
		if mimeType == "" {
			mimeType = content.MIMEType
		}
		if content.Blob == nil {
			hasText = hasText || strings.TrimSpace(content.Text) != ""
			parts = append(parts, content.Text)
			continue
		}
		parts = append(parts, fmt.Sprintf("[resource %s (%s) not inserted]", content.URI, content.MIMEType))
	}
	if !hasText {
		return UserResourceText{}, &UserPromptError{Message: fmt.Sprintf("MCP server %s: resource %s has no text to insert", server.ID, uri)}
	}
	text := strings.Join(parts, "\n\n")
	if len(text) > current.maxResultBytes {
		return UserResourceText{}, &UserPromptError{Message: fmt.Sprintf("MCP server %s: resource %s is %d bytes, more than the server's max_result_bytes (%d)", server.ID, uri, len(text), current.maxResultBytes)}
	}
	return UserResourceText{Server: server.ID, URI: uri, MIMEType: mimeType, Text: text}, nil
}

// resourceURI is the request's URI, or its template filled with the variables. Every
// variable the request gives must be one of the template's; a variable left out is
// undefined, which RFC 6570 expands to nothing.
func resourceURI(request UserResourceRead) (string, error) {
	if (request.URI == "") == (request.URITemplate == "") {
		return "", errors.New("give uri or uri_template, not both")
	}
	if request.URI != "" {
		if len(request.Variables) > 0 {
			return "", errors.New("variables go with uri_template only")
		}
		return request.URI, nil
	}
	template, err := uritemplate.New(request.URITemplate)
	if err != nil {
		return "", fmt.Errorf("uri_template is not an RFC 6570 template: %v", err)
	}
	known := uniqueNames(template.Varnames())
	values := uritemplate.Values{}
	names := make([]string, 0, len(request.Variables))
	for name := range request.Variables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		found := false
		for _, candidate := range known {
			found = found || candidate == name
		}
		if !found {
			return "", fmt.Errorf("uri_template has no variable %s; it has %s", name, strings.Join(known, ", "))
		}
		values.Set(name, uritemplate.String(request.Variables[name]))
	}
	uri, err := template.Expand(values)
	if err != nil {
		return "", fmt.Errorf("fill uri_template: %v", err)
	}
	if strings.TrimSpace(uri) == "" {
		return "", errors.New("the filled uri_template is empty")
	}
	return uri, nil
}

// resourceConnection is the server's connection, checked to offer resources.
func (m *Manager) resourceConnection(ctx context.Context, server sessionfs.MCPServerReference, cwd string) (*connection, error) {
	current, err := m.getConnection(ctx, server, cwd)
	if err != nil {
		return nil, err
	}
	if capabilities := current.capabilities(); capabilities == nil || capabilities.Resources == nil {
		return nil, fmt.Errorf("MCP server %s does not offer resources; turn resources off in the toolset", server.ID)
	}
	return current, nil
}
