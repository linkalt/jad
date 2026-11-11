package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// The remote names of the tools JAD adds for a server's resources and prompts. A slash
// cannot be part of an exposed name, and the exposed name's digest is taken over the remote
// name, so these never share a name with a tool the server itself lists.
const (
	listResourcesName = "resources/list"
	readResourceName  = "resources/read"
	subscribeName     = "resources/subscribe"
	unsubscribeName   = "resources/unsubscribe"
	listPromptsName   = "prompts/list"
	getPromptName     = "prompts/get"
)

const listResourcesSchema = `{"type":"object","properties":{"kind":{"type":"string","enum":["resources","templates"],"description":"resources (the default) lists the resources; templates lists the URI templates a resource URI can be built from."},"cursor":{"type":"string","minLength":1,"description":"next_cursor of the previous page, to continue the list."}},"additionalProperties":false}`

const readResourceSchema = `{"type":"object","properties":{"uri":{"type":"string","minLength":1,"description":"URI of the resource, from the list or built from a template."}},"required":["uri"],"additionalProperties":false}`

const subscribeSchema = `{"type":"object","properties":{"uri":{"type":"string","minLength":1,"description":"URI of the resource to watch, from the list."}},"required":["uri"],"additionalProperties":false}`

const unsubscribeSchema = `{"type":"object","properties":{"uri":{"type":"string","minLength":1,"description":"URI of a resource watched with the subscribe tool."}},"required":["uri"],"additionalProperties":false}`

const listPromptsSchema = `{"type":"object","properties":{"cursor":{"type":"string","minLength":1,"description":"next_cursor of the previous page, to continue the list."}},"additionalProperties":false}`

const getPromptSchema = `{"type":"object","properties":{"name":{"type":"string","minLength":1,"description":"Name of the prompt, from the list."},"arguments":{"type":"object","additionalProperties":{"type":"string"},"description":"The prompt's arguments by name; the list says which it takes and which are required."}},"required":["name"],"additionalProperties":false}`

// featureTools are the tools that reach a server's resources and prompts, as the toolset
// entry turns them on. A server that does not offer a feature the entry asks for is a
// configuration error, so the entry cannot silently do nothing.
func (m *Manager) featureTools(server sessionfs.MCPServerReference, current *connection, sampler Sampler) ([]tools.Tool, error) {
	if !server.Resources && !server.Prompts {
		return nil, nil
	}
	capabilities := current.capabilities()
	route := m.callRoute()
	var loaded []tools.Tool
	add := func(remoteName, schema, description string, execute func(context.Context, *connection, map[string]any) (json.RawMessage, error)) error {
		validator, err := m.schemas.get(json.RawMessage(schema))
		if err != nil {
			return err
		}
		definition := tools.Definition{
			ID:          "mcp:" + server.ID + ":" + remoteName,
			Source:      "mcp",
			Server:      server.ID,
			Name:        exposedToolName(server.ID, remoteName),
			Description: description,
			InputSchema: json.RawMessage(schema),
		}
		loaded = append(loaded, tools.Tool{
			Definition: definition,
			Validator:  validator,
			// Listing and reading resources and prompts change nothing, by the protocol: a
			// plan-mode run keeps these tools, and their calls may run beside others.
			ReadOnly: true,
			ExecuteOutput: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (json.RawMessage, error) {
				if output, routed, err := route.call(ctx, definition, remoteName, arguments, execution); routed {
					return output, err
				}
				// Like a tool call: the server's sampling or elicitation input request in this
				// request's result belongs to the run that made it (sample, elicit).
				ctx = withActiveCall(ctx, &activeCall{request: execution.RequestInteraction, identity: tools.InteractionIdentity{Tool: execution.ToolName, ToolCallID: execution.CallID}, sampler: sampler})
				return execute(ctx, current, arguments)
			},
		})
		return nil
	}
	if server.Resources {
		if capabilities == nil || capabilities.Resources == nil {
			return nil, configError(server.ConfigPath, "MCP server %s does not offer resources; turn resources off in the toolset", server.ID)
		}
		if err := add(listResourcesName, listResourcesSchema, fmt.Sprintf("List the resources (files, documents, records) the MCP server %s offers, a page at a time, or the URI templates it accepts. Read one with the server's read-resource tool.", server.ID), listResources); err != nil {
			return nil, err
		}
		if err := add(readResourceName, readResourceSchema, fmt.Sprintf("Read one resource of the MCP server %s by its URI.", server.ID), readResource); err != nil {
			return nil, err
		}
		if capabilities.Resources.Subscribe && current.receivesNotifications() {
			// Watching changes nothing the person sees: the server only reports changes.
			watch := func(on bool) func(context.Context, *connection, map[string]any) (json.RawMessage, error) {
				return func(ctx context.Context, current *connection, arguments map[string]any) (json.RawMessage, error) {
					return m.subscribeTool(ctx, current, arguments, on)
				}
			}
			if err := add(subscribeName, subscribeSchema, fmt.Sprintf("Watch one resource of the MCP server %s: when it changes, the start of your next turn says so.", server.ID), watch(true)); err != nil {
				return nil, err
			}
			if err := add(unsubscribeName, unsubscribeSchema, fmt.Sprintf("Stop watching a resource of the MCP server %s.", server.ID), watch(false)); err != nil {
				return nil, err
			}
		}
	}
	if server.Prompts {
		if capabilities == nil || capabilities.Prompts == nil {
			return nil, configError(server.ConfigPath, "MCP server %s does not offer prompts; turn prompts off in the toolset", server.ID)
		}
		if err := add(listPromptsName, listPromptsSchema, fmt.Sprintf("List the prompt templates the MCP server %s offers, with their arguments, a page at a time.", server.ID), listPrompts); err != nil {
			return nil, err
		}
		if err := add(getPromptName, getPromptSchema, fmt.Sprintf("Get one prompt template of the MCP server %s filled with its arguments, as the messages it expands to.", server.ID), getPrompt); err != nil {
			return nil, err
		}
	}
	return loaded, nil
}

// capabilities are what the server announced when the connection started.
func (c *connection) capabilities() *mcpSDK.ServerCapabilities {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.session == nil {
		return nil
	}
	result := c.session.InitializeResult()
	if result == nil {
		return nil
	}
	return result.Capabilities
}

// request runs one request on the session under the same rules as a tool call: one at a
// time per connection, bounded by the server's timeout, and stopped by close. A request a run's
// tool makes (a resource or prompt tool) is the connection's call while it runs, as callTool's
// is; one whose context carries no run's call (the user's resource and prompt requests, a
// resubscribe or the log level outside a call) leaves the connection with none, so a server's
// sampling request meanwhile is refused. The request's context carries the call (or none) to the
// input requests of its result (attributedCall).
func (c *connection) request(ctx context.Context, send func(context.Context, *mcpSDK.ClientSession) error) error {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return errors.New("MCP connection is closed")
	}
	call := activeCallFrom(ctx)
	callCtx, cancel := context.WithTimeout(c.ctx, c.timeout)
	defer cancel()
	callCtx = withInlineRequest(callCtx, call)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	return send(callCtx, c.session)
}

// featureError is the result of a resource or prompt request the server refused or did
// not answer. Like a failed tool call, it names only the kind of failure.
func featureError(serverID, operation string, err error) (json.RawMessage, error) {
	encoded, encodeErr := tools.MarshalJSON(struct {
		Status    string `json:"status"`
		Server    string `json:"server"`
		Operation string `json:"operation"`
		Error     string `json:"error"`
	}{Status: "error", Server: serverID, Operation: operation, Error: safeErrorType(err)})
	if encodeErr != nil {
		return nil, fmt.Errorf("encode MCP %s error: %w", operation, encodeErr)
	}
	return marshalParts([]outputPart{textPart(string(encoded))})
}

// jsonResult is a listing as one text part, cut to max_result_bytes like any result.
func jsonResult(value any, maxBytes int) (json.RawMessage, error) {
	encoded, err := tools.MarshalJSON(value)
	if err != nil {
		return nil, fmt.Errorf("encode MCP listing: %w", err)
	}
	return marshalParts(boundedContent(&mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: string(encoded)}}}, maxBytes))
}

func stringArgument(arguments map[string]any, name string) string {
	value, _ := arguments[name].(string)
	return value
}

type resourceEntry struct {
	URI         string `json:"uri,omitempty"`
	URITemplate string `json:"uri_template,omitempty"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mime_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

type resourceListing struct {
	Server     string          `json:"server"`
	Kind       string          `json:"kind"`
	Resources  []resourceEntry `json:"resources"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func listResources(ctx context.Context, current *connection, arguments map[string]any) (json.RawMessage, error) {
	serverID := current.serverID
	kind := stringArgument(arguments, "kind")
	if kind == "" {
		kind = "resources"
	}
	cursor := stringArgument(arguments, "cursor")
	listing := resourceListing{Server: serverID, Kind: kind, Resources: []resourceEntry{}}
	err := current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
		if kind == "templates" {
			result, err := session.ListResourceTemplates(ctx, &mcpSDK.ListResourceTemplatesParams{Cursor: cursor})
			if err != nil {
				return err
			}
			for _, template := range result.ResourceTemplates {
				if template != nil {
					listing.Resources = append(listing.Resources, resourceEntry{URITemplate: template.URITemplate, Name: template.Name, Title: template.Title, Description: template.Description, MIMEType: template.MIMEType})
				}
			}
			listing.NextCursor = result.NextCursor
			return nil
		}
		result, err := session.ListResources(ctx, &mcpSDK.ListResourcesParams{Cursor: cursor})
		if err != nil {
			return err
		}
		for _, resource := range result.Resources {
			if resource != nil {
				listing.Resources = append(listing.Resources, resourceEntry{URI: resource.URI, Name: resource.Name, Title: resource.Title, Description: resource.Description, MIMEType: resource.MIMEType, Size: resource.Size})
			}
		}
		listing.NextCursor = result.NextCursor
		return nil
	})
	if err != nil {
		return featureError(serverID, listResourcesName, err)
	}
	return jsonResult(listing, current.maxResultBytes)
}

// readResource returns the resource's contents the way an embedded resource in a tool
// result is returned: text as text, an image as an image, other binary data as a file.
func readResource(ctx context.Context, current *connection, arguments map[string]any) (json.RawMessage, error) {
	uri := stringArgument(arguments, "uri")
	var contents []*mcpSDK.ResourceContents
	err := current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
		result, err := session.ReadResource(ctx, &mcpSDK.ReadResourceParams{URI: uri})
		if err != nil {
			return err
		}
		contents = result.Contents
		return nil
	})
	if err != nil {
		return featureError(current.serverID, readResourceName, err)
	}
	result := &mcpSDK.CallToolResult{Content: make([]mcpSDK.Content, 0, len(contents))}
	for _, content := range contents {
		if content != nil {
			result.Content = append(result.Content, &mcpSDK.EmbeddedResource{Resource: content})
		}
	}
	if len(result.Content) == 0 {
		result.Content = append(result.Content, &mcpSDK.TextContent{Text: "The MCP resource " + uri + " has no contents."})
	}
	return marshalParts(boundedContent(result, current.maxResultBytes))
}

type promptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type promptEntry struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []promptArgument `json:"arguments,omitempty"`
}

type promptListing struct {
	Server     string        `json:"server"`
	Prompts    []promptEntry `json:"prompts"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

func listPrompts(ctx context.Context, current *connection, arguments map[string]any) (json.RawMessage, error) {
	listing := promptListing{Server: current.serverID, Prompts: []promptEntry{}}
	err := current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
		result, err := session.ListPrompts(ctx, &mcpSDK.ListPromptsParams{Cursor: stringArgument(arguments, "cursor")})
		if err != nil {
			return err
		}
		for _, prompt := range result.Prompts {
			if prompt == nil {
				continue
			}
			entry := promptEntry{Name: prompt.Name, Title: prompt.Title, Description: prompt.Description}
			for _, argument := range prompt.Arguments {
				if argument != nil {
					entry.Arguments = append(entry.Arguments, promptArgument{Name: argument.Name, Description: argument.Description, Required: argument.Required})
				}
			}
			listing.Prompts = append(listing.Prompts, entry)
		}
		listing.NextCursor = result.NextCursor
		return nil
	})
	if err != nil {
		return featureError(current.serverID, listPromptsName, err)
	}
	return jsonResult(listing, current.maxResultBytes)
}

// getPrompt returns the messages the prompt expands to. Each message starts with a line
// naming its role, followed by its content as a tool result would show it.
func getPrompt(ctx context.Context, current *connection, arguments map[string]any) (json.RawMessage, error) {
	params := &mcpSDK.GetPromptParams{Name: stringArgument(arguments, "name")}
	if values, ok := arguments["arguments"].(map[string]any); ok && len(values) > 0 {
		params.Arguments = make(map[string]string, len(values))
		for name, value := range values {
			text, _ := value.(string)
			params.Arguments[name] = text
		}
	}
	var prompt *mcpSDK.GetPromptResult
	err := current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
		result, err := session.GetPrompt(ctx, params)
		prompt = result
		return err
	})
	if err != nil {
		return featureError(current.serverID, getPromptName, err)
	}
	result := &mcpSDK.CallToolResult{}
	if prompt != nil && prompt.Description != "" {
		result.Content = append(result.Content, &mcpSDK.TextContent{Text: "Prompt: " + prompt.Description})
	}
	if prompt != nil {
		for _, message := range prompt.Messages {
			if message == nil {
				continue
			}
			result.Content = append(result.Content, &mcpSDK.TextContent{Text: "[" + string(message.Role) + "]"})
			if message.Content != nil {
				result.Content = append(result.Content, message.Content)
			}
		}
	}
	if len(result.Content) == 0 {
		result.Content = append(result.Content, &mcpSDK.TextContent{Text: "The MCP prompt " + params.Name + " has no messages."})
	}
	return marshalParts(boundedContent(result, current.maxResultBytes))
}

// subscribeTool watches (on) or stops watching one resource for the model.
func (m *Manager) subscribeTool(ctx context.Context, current *connection, arguments map[string]any, on bool) (json.RawMessage, error) {
	uri := stringArgument(arguments, "uri")
	operation := "subscribe"
	if !on {
		operation = "unsubscribe"
	}
	if err := subscribe(ctx, m, current, uri, on); err != nil {
		return featureError(current.serverID, "resources/"+operation, err)
	}
	return jsonResult(struct {
		URI        string `json:"uri"`
		Subscribed bool   `json:"subscribed"`
	}{uri, on}, current.maxResultBytes)
}
