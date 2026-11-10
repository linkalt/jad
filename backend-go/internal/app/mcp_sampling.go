package app

import (
	"context"
	"encoding/json"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	mcpclient "jad/backend-go/internal/tools/mcp"
	"jad/backend-go/internal/tools/native"
)

// mcpSampler answers the sampling requests of the session's MCP servers with the
// session's model, built like a run's client (credential, fallback, pricing). The
// request is bounded by the server's own timeout through ctx and by the provider stream
// settings; nothing of it is stored in the transcript.
func (r *Resolver) mcpSampler(config sessionfs.SessionConfig) mcpclient.Sampler {
	return func(ctx context.Context, request mcpclient.SamplingRequest) (string, string, error) {
		apiKey, err := r.providerAPIKey(config)
		if err != nil {
			return "", "", err
		}
		streamer, _ := r.runStreamer(config, apiKey)
		messages := make([]session.TextMessage, 0, len(request.Messages))
		for _, message := range request.Messages {
			messages = append(messages, session.TextMessage{Role: message.Role, Text: message.Text})
		}
		// No timeout of its own: the provider stream settings bound the request, and the
		// server's call that asked waits for it within its own timeout.
		text, err := session.CompleteText(ctx, streamer, runnerModelSettings(config), request.SystemPrompt, messages, int64(request.MaxOutputTokens), 0)
		if err != nil {
			return "", "", err
		}
		if r.logger != nil {
			r.logger.Info("mcp sampling", "server", request.Server, "profile", config.Profile, "messages", len(messages))
		}
		return text, config.Model.Name, nil
	}
}

// mcpSandboxWrapper returns the key and wrapper that start the session's stdio MCP servers
// in its sandbox when sandbox.tools lists mcp, and an empty key and nil otherwise.
func mcpSandboxWrapper(config sessionfs.SessionConfig) (string, mcpclient.CommandWrapper) {
	sandbox := config.Sandbox
	if !sandbox.Covers("mcp") {
		return "", nil
	}
	key, err := json.Marshal(sandbox)
	if err != nil {
		return "", nil
	}
	return string(key) + "\x00" + config.WorkingDir, sandboxCommandWrapper(sandbox, config.WorkingDir)
}

// sandboxCommandWrapper starts a stdio server in sandbox (nil: none) for a run that works in
// workspace. A workspace that is an overlay run's worktree joins the run's holder, with or
// without a sandbox.
func sandboxCommandWrapper(sandbox *sessionfs.SandboxSettings, workspace string) mcpclient.CommandWrapper {
	return func(program string, args []string, cwd string) (mcpclient.CommandLaunch, error) {
		file, argv0, arguments, dir, err := native.SandboxCommand(sandbox, workspace, cwd, program, args)
		if err != nil {
			return mcpclient.CommandLaunch{}, err
		}
		return mcpclient.CommandLaunch{File: file, Argv0: argv0, Args: arguments, Dir: dir}, nil
	}
}
