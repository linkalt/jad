package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
)

// The slash menu's view of MCP prompts. A toolset entry with prompts: true offers its
// server's prompts twice: as tools to the model (featureTools) and here, to the person
// typing, who picks one and gets its text in the draft. Both go through the same
// connection, one request at a time, under the server's timeout_ms.

// UserPromptArgument is one argument a prompt takes.
type UserPromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// UserPrompt is one prompt template a server offers.
type UserPrompt struct {
	Server      string               `json:"server"`
	Name        string               `json:"name"`
	Title       string               `json:"title,omitempty"`
	Description string               `json:"description,omitempty"`
	Arguments   []UserPromptArgument `json:"arguments"`
}

// UserPromptFailure is a server whose prompts could not be listed. The other servers'
// prompts are still listed.
type UserPromptFailure struct {
	Server string `json:"server"`
	Error  string `json:"error"`
}

// UserPromptList is every prompt of the servers with prompts: true, in toolset order.
type UserPromptList struct {
	Prompts  []UserPrompt        `json:"prompts"`
	Failures []UserPromptFailure `json:"failures,omitempty"`
}

// UserPromptText is a prompt filled with its arguments, as text for the draft.
type UserPromptText struct {
	Server      string `json:"server"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Text        string `json:"text"`
}

// UserPromptError is a prompt or resource request that could not be answered. NotFound is
// a server that is not in the toolset with prompts (or resources): true; Invalid is a
// request that cannot be sent as given; any other failure is the server's.
type UserPromptError struct {
	NotFound bool
	Invalid  bool
	Message  string
}

func (e *UserPromptError) Error() string { return e.Message }

// promptServers are the enabled servers whose toolset entry turns prompts on.
func promptServers(servers []sessionfs.MCPServerReference) []sessionfs.MCPServerReference {
	selected := make([]sessionfs.MCPServerReference, 0, len(servers))
	for _, server := range servers {
		if server.Enabled && server.Prompts {
			selected = append(selected, server)
		}
	}
	return selected
}

// ListUserPrompts lists the prompts of every server with prompts: true. Servers are asked
// at the same time; a server that fails is reported in Failures and the rest are listed.
// Each server's list stops at its max_tools, the budget its tool list has too.
func (m *Manager) ListUserPrompts(ctx context.Context, servers []sessionfs.MCPServerReference, cwd string) (UserPromptList, error) {
	if ctx == nil {
		return UserPromptList{}, errors.New("MCP prompt listing context is required")
	}
	selected := promptServers(servers)
	type serverResult struct {
		prompts []UserPrompt
		err     string
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
			prompts, err := m.listServerPrompts(ctx, server, cwd)
			if err != nil {
				results[index].err = err.Error()
				return
			}
			results[index].prompts = prompts
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return UserPromptList{}, err
	}
	list := UserPromptList{Prompts: []UserPrompt{}}
	for index, result := range results {
		if result.err != "" {
			list.Failures = append(list.Failures, UserPromptFailure{Server: selected[index].ID, Error: result.err})
			continue
		}
		list.Prompts = append(list.Prompts, result.prompts...)
	}
	return list, nil
}

func (m *Manager) listServerPrompts(ctx context.Context, server sessionfs.MCPServerReference, cwd string) ([]UserPrompt, error) {
	current, err := m.promptConnection(ctx, server, cwd)
	if err != nil {
		return nil, err
	}
	prompts := []UserPrompt{}
	cursor := ""
	seenCursors := map[string]struct{}{}
	for {
		var page *mcpSDK.ListPromptsResult
		err := current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
			result, err := session.ListPrompts(ctx, &mcpSDK.ListPromptsParams{Cursor: cursor})
			page = result
			return err
		})
		if err != nil {
			return nil, errors.New(userRequestError(err, current))
		}
		if page == nil {
			return prompts, nil
		}
		for _, prompt := range page.Prompts {
			if prompt == nil || strings.TrimSpace(prompt.Name) == "" {
				continue
			}
			if len(prompts) == current.maxTools {
				return nil, fmt.Errorf("the server lists more than max_tools (%d) prompts", current.maxTools)
			}
			entry := UserPrompt{
				Server:      server.ID,
				Name:        prompt.Name,
				Title:       cutText(prompt.Title, current.maxDescriptionBytes),
				Description: cutText(prompt.Description, current.maxDescriptionBytes),
				Arguments:   []UserPromptArgument{},
			}
			for _, argument := range prompt.Arguments {
				if argument != nil && argument.Name != "" {
					entry.Arguments = append(entry.Arguments, UserPromptArgument{
						Name:        argument.Name,
						Description: cutText(argument.Description, current.maxDescriptionBytes),
						Required:    argument.Required,
					})
				}
			}
			prompts = append(prompts, entry)
		}
		if page.NextCursor == "" {
			return prompts, nil
		}
		if _, repeated := seenCursors[page.NextCursor]; repeated {
			return nil, errors.New("the server repeated a prompts/list cursor")
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
}

// GetUserPrompt fills one prompt of a server with prompts: true and returns its messages as
// text. A text that does not fit the server's max_result_bytes is refused, not cut: a cut
// prompt would be sent as if it were whole.
func (m *Manager) GetUserPrompt(ctx context.Context, servers []sessionfs.MCPServerReference, cwd, serverID, name string, arguments map[string]string) (UserPromptText, error) {
	if ctx == nil {
		return UserPromptText{}, errors.New("MCP prompt context is required")
	}
	var server *sessionfs.MCPServerReference
	for _, candidate := range promptServers(servers) {
		if candidate.ID == serverID {
			server = &candidate
			break
		}
	}
	if server == nil {
		return UserPromptText{}, &UserPromptError{NotFound: true, Message: fmt.Sprintf("no MCP server %q with prompts: true in the profile's toolsets", serverID)}
	}
	current, err := m.promptConnection(ctx, *server, cwd)
	if err != nil {
		return UserPromptText{}, &UserPromptError{Message: err.Error()}
	}
	params := &mcpSDK.GetPromptParams{Name: name}
	if len(arguments) > 0 {
		params.Arguments = arguments
	}
	var prompt *mcpSDK.GetPromptResult
	err = current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
		result, err := session.GetPrompt(ctx, params)
		prompt = result
		return err
	})
	if err != nil {
		return UserPromptText{}, &UserPromptError{Message: fmt.Sprintf("MCP server %s: prompt %s: %s", server.ID, name, userRequestError(err, current))}
	}
	text := promptText(prompt)
	if strings.TrimSpace(text) == "" {
		return UserPromptText{}, &UserPromptError{Message: fmt.Sprintf("MCP server %s: prompt %s has no text", server.ID, name)}
	}
	if len(text) > current.maxResultBytes {
		return UserPromptText{}, &UserPromptError{Message: fmt.Sprintf("MCP server %s: prompt %s is %d bytes, more than the server's max_result_bytes (%d)", server.ID, name, len(text), current.maxResultBytes)}
	}
	return UserPromptText{Server: server.ID, Name: name, Description: cutText(prompt.Description, current.maxDescriptionBytes), Text: text}, nil
}

// promptConnection is the server's connection, checked to offer prompts.
func (m *Manager) promptConnection(ctx context.Context, server sessionfs.MCPServerReference, cwd string) (*connection, error) {
	current, err := m.getConnection(ctx, server, cwd)
	if err != nil {
		return nil, err
	}
	if capabilities := current.capabilities(); capabilities == nil || capabilities.Prompts == nil {
		return nil, fmt.Errorf("MCP server %s does not offer prompts; turn prompts off in the toolset", server.ID)
	}
	return current, nil
}

// promptText is the prompt's messages as one text. When every message is the user's, the
// texts are joined by blank lines; otherwise each message starts with a [role] line, so an
// assistant turn the prompt carries is not passed off as the user's words. Content that is
// not text is named, not inserted.
func promptText(prompt *mcpSDK.GetPromptResult) string {
	if prompt == nil {
		return ""
	}
	labelled := false
	for _, message := range prompt.Messages {
		if message != nil && message.Role != "user" {
			labelled = true
		}
	}
	parts := make([]string, 0, len(prompt.Messages))
	for _, message := range prompt.Messages {
		if message == nil {
			continue
		}
		text := contentText(message.Content)
		if labelled {
			text = "[" + string(message.Role) + "]\n" + text
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
}

func contentText(content mcpSDK.Content) string {
	switch value := content.(type) {
	case nil:
		return ""
	case *mcpSDK.TextContent:
		return value.Text
	case *mcpSDK.EmbeddedResource:
		if value.Resource != nil && value.Resource.Blob == nil {
			return value.Resource.Text
		}
		if value.Resource != nil {
			return fmt.Sprintf("[resource %s (%s) not inserted]", value.Resource.URI, value.Resource.MIMEType)
		}
		return ""
	case *mcpSDK.ResourceLink:
		return value.URI
	case *mcpSDK.ImageContent:
		return fmt.Sprintf("[image (%s) not inserted]", value.MIMEType)
	case *mcpSDK.AudioContent:
		return fmt.Sprintf("[audio (%s) not inserted]", value.MIMEType)
	default:
		return "[content not inserted]"
	}
}

// userRequestError says why a request failed, for the person who asked. Unlike a tool
// result, which names only the kind of failure, it carries the server's own refusal
// message (cut to max_description_bytes), since the person needs it to fix the arguments.
func userRequestError(err error, current *connection) string {
	var rpcError *jsonrpc.Error
	var tooLarge *responseTooLargeError
	switch {
	case errors.As(err, &tooLarge):
		return fmt.Sprintf("the answer is larger than the server's max_result_bytes (%d)", tooLarge.limit)
	case strings.Contains(err.Error(), "SSE event exceeded"):
		// The SDK cuts an event-stream answer at MaxEventSize, which is max_result_bytes,
		// and says so only in its error text.
		return fmt.Sprintf("the answer is larger than the server's max_result_bytes (%d)", current.maxResultBytes)
	case errors.As(err, &rpcError):
		return "the server refused: " + cutText(rpcError.Message, current.maxDescriptionBytes)
	case errors.Is(err, context.DeadlineExceeded):
		return "the server did not answer within timeout_ms"
	case errors.Is(err, mcpSDK.ErrConnectionClosed):
		return "the connection to the server closed"
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	default:
		return "the request failed (" + safeErrorType(err) + ")"
	}
}

// cutText cuts text to at most maxBytes bytes on a rune boundary.
func cutText(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
