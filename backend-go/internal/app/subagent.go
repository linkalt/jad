package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

	"example.com/agent-harness/backend-go/internal/session"
	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools/native"
	"example.com/agent-harness/backend-go/internal/transcript"
)

var unsafeDirectoryCharacters = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// subagentLauncher runs the children of the subagent tool. It holds the calling
// session's configuration, so a child that names no model of its own uses the
// parent's model, credential and retry settings.
type subagentLauncher struct {
	resolver *Resolver
	parent   sessionfs.SessionConfig
}

// subagentLauncherFor returns the launcher of one session, or nil when the session
// does not configure the subagent tool.
func (r *Resolver) subagentLauncherFor(config sessionfs.SessionConfig) native.SubagentLauncher {
	if config.NativeSubagent == nil {
		return nil
	}
	return subagentLauncher{resolver: r, parent: config}
}

// Run starts one child with a fresh transcript and returns its final answer. The
// child is an ordinary session.Runner run: its own store, its own model client and
// the read-only catalog the tool chose, with no approval, checkpoint or state
// context, because it cannot change anything.
func (l subagentLauncher) Run(ctx context.Context, request native.SubagentRequest) (native.SubagentResult, error) {
	config, err := l.childConfig(request)
	if err != nil {
		return native.SubagentResult{}, err
	}
	apiKey, err := l.resolver.providerAPIKey(config)
	if err != nil {
		return native.SubagentResult{}, err
	}
	store, dir, err := l.openStore(config, request)
	if err != nil {
		return native.SubagentResult{}, err
	}
	if request.OnTranscript != nil {
		request.OnTranscript(dir)
	}
	runner, err := session.NewRunner(store, l.resolver.newProviderStreamer(config, apiKey), session.ModelSettings{
		Model:       config.Model.Name,
		Request:     config.Model.Request,
		HostedTools: config.Model.HostedTools,
		PromptRole:  config.Model.PromptRole,
		PromptText:  config.Model.PromptText,
	})
	if err != nil {
		return native.SubagentResult{}, fmt.Errorf("create sub-agent runner: %w", err)
	}
	input, err := json.Marshal(request.Task)
	if err != nil {
		return native.SubagentResult{}, fmt.Errorf("encode sub-agent task: %w", err)
	}

	var rounds, tokens atomic.Int64
	// lastText is the newest assistant text of any round. A child that is cut off has
	// no final answer, but what it wrote last is the best partial work it has.
	var lastText atomic.Value
	result, runErr := runner.Run(ctx, input, session.RunOptions{
		ToolCatalog:          request.Tools,
		ImageResizer:         newImageResizer(config.ImageResize),
		ImageStore:           newImageStore(config.ImageStore),
		ToolExecution:        request.Execution,
		MaxToolRounds:        int(request.Agent.MaxToolRounds),
		MaxRunTokens:         request.Agent.MaxRunTokens,
		MaxParallelToolCalls: int(l.parent.MaxParallelToolCalls),
		ToolCallScheduling:   l.parent.ToolCallScheduling,
		AcceptedRequestRole:  l.parent.StateContext.Role,
		OnResponseCommitted: func(event json.RawMessage, _ transcript.CommitResult) {
			rounds.Add(1)
			tokens.Add(session.ResponseTokens(event))
			if text := native.SubagentAnswerText(event); strings.TrimSpace(text) != "" {
				lastText.Store(text)
			}
		},
	})
	outcome := native.SubagentResult{Rounds: int(rounds.Load()), Tokens: tokens.Load()}
	if runErr != nil {
		outcome.Text, _ = lastText.Load().(string)
		return outcome, runErr
	}
	outcome.Text = native.SubagentAnswerText(result.Event)
	return outcome, nil
}

// childConfig is the parent's configuration with the agent's model and instructions.
func (l subagentLauncher) childConfig(request native.SubagentRequest) (sessionfs.SessionConfig, error) {
	config := l.parent
	if request.Agent.ModelPath != "" {
		config.ModelPath = request.Agent.ModelPath
	}
	// The parent's prompt is not the child's: it gets its own instructions and
	// documents, or none.
	config.Model.PromptText = request.Agent.PromptText
	loaded, err := config.WithModel()
	if err != nil {
		return sessionfs.SessionConfig{}, fmt.Errorf("load the model of agent %s: %w", request.Agent.Name, err)
	}
	return loaded, nil
}

// openStore creates the child's transcript under the agent storage directory, in a
// directory of its own so two children never share history.
func (l subagentLauncher) openStore(config sessionfs.SessionConfig, request native.SubagentRequest) (*transcript.TurnStore, string, error) {
	if config.NativeSubagent == nil {
		return nil, "", errors.New("tools.native.subagent is not configured")
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, "", fmt.Errorf("create sub-agent directory name: %w", err)
	}
	name := unsafeDirectoryCharacters.ReplaceAllString(request.CallID, "_")
	if name == "" {
		name = "call"
	}
	directory := filepath.Join(config.NativeSubagent.StorageDir, name+"-"+hex.EncodeToString(random[:]))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, "", fmt.Errorf("create sub-agent directory: %w", err)
	}
	journal, err := transcript.NewJournal(filepath.Join(directory, "messages.jsonl"), transcript.DurabilityMode(config.Persistence.Durability))
	if err != nil {
		return nil, "", fmt.Errorf("create sub-agent transcript: %w", err)
	}
	store, err := transcript.NewTurnStore(journal, filepath.Join(directory, "staging"), transcript.TurnStoreOptions{
		Logger:           l.resolver.logger,
		MaxItemBytes:     config.Persistence.MaxItemBytes,
		ResponseMetadata: transcript.ResponseMetadataMode(config.Persistence.ResponseMetadata),
	})
	if err != nil {
		return nil, "", fmt.Errorf("create sub-agent turn store: %w", err)
	}
	return store, directory, nil
}
