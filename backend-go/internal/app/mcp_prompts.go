package app

import (
	"context"
	"errors"
	"net/http"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	mcpclient "jad/backend-go/internal/tools/mcp"
)

// promptSessionConfig resolves the session profile whose MCP prompts are asked for.
func (r *Resolver) promptSessionConfig(ctx context.Context, reference, profile string) (sessionfs.SessionConfig, *mcpclient.Manager, error) {
	if ctx == nil {
		return sessionfs.SessionConfig{}, nil, errors.New("session resolution context is required")
	}
	if err := ctx.Err(); err != nil {
		return sessionfs.SessionConfig{}, nil, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return sessionfs.SessionConfig{}, nil, mapSessionError(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return sessionfs.SessionConfig{}, nil, mapSessionError(err)
	}
	// The profile's own manager: a prompt request shares the connection its runs use.
	manager, err := r.sharedMCPManager(config)
	if err != nil {
		return sessionfs.SessionConfig{}, nil, err
	}
	return config, manager, nil
}

// ListMCPPrompts implements httpapi.SessionMCPPrompts.
func (r *Resolver) ListMCPPrompts(ctx context.Context, reference, profile string) (httpapi.MCPPromptList, error) {
	config, manager, err := r.promptSessionConfig(ctx, reference, profile)
	if err != nil {
		return httpapi.MCPPromptList{}, err
	}
	listed, err := manager.ListUserPrompts(ctx, config.MCPServers, config.WorkingDir)
	if err != nil {
		return httpapi.MCPPromptList{}, err
	}
	result := httpapi.MCPPromptList{Prompts: make([]httpapi.MCPPrompt, 0, len(listed.Prompts))}
	for _, prompt := range listed.Prompts {
		arguments := make([]httpapi.MCPPromptArgument, 0, len(prompt.Arguments))
		for _, argument := range prompt.Arguments {
			arguments = append(arguments, httpapi.MCPPromptArgument(argument))
		}
		result.Prompts = append(result.Prompts, httpapi.MCPPrompt{
			Server:      prompt.Server,
			Name:        prompt.Name,
			Title:       prompt.Title,
			Description: prompt.Description,
			Arguments:   arguments,
		})
	}
	for _, failure := range listed.Failures {
		result.Failures = append(result.Failures, httpapi.MCPPromptFailure(failure))
	}
	return result, nil
}

// GetMCPPrompt implements httpapi.SessionMCPPrompts. A server outside the profile's
// toolsets is 404; a server that cannot be reached or refuses the prompt is 502.
func (r *Resolver) GetMCPPrompt(ctx context.Context, reference, profile string, request httpapi.MCPPromptRequest) (httpapi.MCPPromptText, error) {
	config, manager, err := r.promptSessionConfig(ctx, reference, profile)
	if err != nil {
		return httpapi.MCPPromptText{}, err
	}
	filled, err := manager.GetUserPrompt(ctx, config.MCPServers, config.WorkingDir, request.Server, request.Name, request.Arguments)
	if err != nil {
		var promptErr *mcpclient.UserPromptError
		if errors.As(err, &promptErr) {
			status := http.StatusBadGateway
			if promptErr.NotFound {
				status = http.StatusNotFound
			}
			return httpapi.MCPPromptText{}, &httpapi.StatusError{Code: status, Message: promptErr.Message}
		}
		return httpapi.MCPPromptText{}, err
	}
	return httpapi.MCPPromptText(filled), nil
}
