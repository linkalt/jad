package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/tools"
)

// What the client offers a server beyond tools and resources, set in the server file:
//
//	"roots": "none" (default) or "workspace"
//	"changes_note": false (default) or true
//	"server_log": "off" (default), "error", "warning", "info" or "debug"
//	"sampling": {"enabled": false, "max_output_tokens": 1024, "max_requests": 20}
//	"elicitation": {"enabled": false, "timeout_ms": 300000, "max_draft_bytes": 65536, "max_draft_depth": 8}
const (
	RootsWorkspace = "workspace"
	RootsNone      = "none"

	ServerLogOff = "off"

	defaultSamplingMaxOutputTokens = 1024
	maxSamplingMaxOutputTokens     = 128_000
	defaultSamplingMaxRequests     = 20
	maxSamplingMaxRequests         = 10_000
	defaultElicitationTimeoutMS    = 300_000
	maxElicitationTimeoutMS        = 3_600_000
	defaultElicitationDraftBytes   = 65_536
	maxElicitationDraftBytes       = 1_048_576
	defaultElicitationDraftDepth   = 8
	maxElicitationDraftDepth       = 64
	// maxElicitationTitleBytes cuts the first line of the server's message shown as the
	// question's title.
	maxElicitationTitleBytes = 120
	// maxServerLogBytes cuts one log message a server sends before it reaches the log.
	defaultServerLogMaxBytes = 2048
	maxServerLogMaxBytes     = 65_536

	// ResourceEventToolsChanged and ResourceEventPromptsChanged are a server whose tool or
	// prompt list changed; the next run lists the tools again.
	ResourceEventToolsChanged   = "tools_changed"
	ResourceEventPromptsChanged = "prompts_changed"
)

var serverLogLevels = map[string]slog.Level{"error": slog.LevelError, "warning": slog.LevelWarn, "info": slog.LevelInfo, "debug": slog.LevelDebug}

// clientFeatures are the client capabilities of one server.
type clientFeatures struct {
	Roots string `json:"roots"`
	// ChangesNote tells the profile's next run what the server reported changed (its
	// resource, tool or prompt list, a watched resource). Off by default; the notifications
	// are kept either way for GET .../mcp-resources/events. Left out of the fingerprint
	// when off, so a server file without the key keeps its connection fingerprint.
	ChangesNote       bool   `json:"changes_note,omitempty"`
	ServerLog         string `json:"server_log"`
	ServerLogMaxBytes int    `json:"server_log_max_bytes"`
	Sampling          bool   `json:"sampling"`
	SamplingMaxTokens int    `json:"sampling_max_output_tokens"`
	SamplingMaxCalls  int    `json:"sampling_max_requests"`
	Elicitation       bool   `json:"elicitation"`
	ElicitTimeoutMS   int64  `json:"elicitation_timeout_ms"`
	ElicitDraftBytes  int64  `json:"elicitation_max_draft_bytes"`
	ElicitDraftDepth  int64  `json:"elicitation_max_draft_depth"`
}

func normalizeClientFeatures(fields map[string]json.RawMessage, path string) (clientFeatures, error) {
	features := clientFeatures{Roots: RootsNone, ServerLog: ServerLogOff, ServerLogMaxBytes: defaultServerLogMaxBytes, SamplingMaxTokens: defaultSamplingMaxOutputTokens, SamplingMaxCalls: defaultSamplingMaxRequests,
		ElicitTimeoutMS: defaultElicitationTimeoutMS, ElicitDraftBytes: defaultElicitationDraftBytes, ElicitDraftDepth: defaultElicitationDraftDepth}
	if raw, exists := fields["roots"]; exists {
		if json.Unmarshal(raw, &features.Roots) != nil || (features.Roots != RootsWorkspace && features.Roots != RootsNone) {
			return clientFeatures{}, configError(path, "roots must be \"workspace\" or \"none\"")
		}
	}
	if raw, exists := fields["changes_note"]; exists && json.Unmarshal(raw, &features.ChangesNote) != nil {
		return clientFeatures{}, configError(path, "changes_note must be a boolean")
	}
	if raw, exists := fields["server_log"]; exists {
		known := json.Unmarshal(raw, &features.ServerLog) == nil
		if known {
			_, known = serverLogLevels[features.ServerLog]
			known = known || features.ServerLog == ServerLogOff
		}
		if !known {
			return clientFeatures{}, configError(path, "server_log must be \"off\", \"error\", \"warning\", \"info\" or \"debug\"")
		}
	}
	maxBytes, err := configuredInteger(fields["server_log_max_bytes"], defaultServerLogMaxBytes, 64, maxServerLogMaxBytes, path, "server_log_max_bytes")
	if err != nil {
		return clientFeatures{}, err
	}
	features.ServerLogMaxBytes = int(maxBytes)
	if raw, exists := fields["sampling"]; exists {
		sampling, err := objectFields(raw, path, "sampling must be an object")
		if err != nil {
			return clientFeatures{}, err
		}
		for key := range sampling {
			if key != "enabled" && key != "max_output_tokens" && key != "max_requests" {
				return clientFeatures{}, configError(path, "sampling.%s is not supported", key)
			}
		}
		if raw, exists := sampling["enabled"]; exists && json.Unmarshal(raw, &features.Sampling) != nil {
			return clientFeatures{}, configError(path, "sampling.enabled must be a boolean")
		}
		tokens, err := configuredInteger(sampling["max_output_tokens"], defaultSamplingMaxOutputTokens, 1, maxSamplingMaxOutputTokens, path, "sampling.max_output_tokens")
		if err != nil {
			return clientFeatures{}, err
		}
		calls, err := configuredInteger(sampling["max_requests"], defaultSamplingMaxRequests, 1, maxSamplingMaxRequests, path, "sampling.max_requests")
		if err != nil {
			return clientFeatures{}, err
		}
		features.SamplingMaxTokens, features.SamplingMaxCalls = int(tokens), int(calls)
	}
	if raw, exists := fields["elicitation"]; exists {
		elicitation, err := objectFields(raw, path, "elicitation must be an object")
		if err != nil {
			return clientFeatures{}, err
		}
		for key := range elicitation {
			if key != "enabled" && key != "timeout_ms" && key != "max_draft_bytes" && key != "max_draft_depth" {
				return clientFeatures{}, configError(path, "elicitation.%s is not supported", key)
			}
		}
		if raw, exists := elicitation["enabled"]; exists && json.Unmarshal(raw, &features.Elicitation) != nil {
			return clientFeatures{}, configError(path, "elicitation.enabled must be a boolean")
		}
		if features.ElicitTimeoutMS, err = configuredInteger(elicitation["timeout_ms"], defaultElicitationTimeoutMS, 1000, maxElicitationTimeoutMS, path, "elicitation.timeout_ms"); err != nil {
			return clientFeatures{}, err
		}
		if features.ElicitDraftBytes, err = configuredInteger(elicitation["max_draft_bytes"], defaultElicitationDraftBytes, 64, maxElicitationDraftBytes, path, "elicitation.max_draft_bytes"); err != nil {
			return clientFeatures{}, err
		}
		if features.ElicitDraftDepth, err = configuredInteger(elicitation["max_draft_depth"], defaultElicitationDraftDepth, 2, maxElicitationDraftDepth, path, "elicitation.max_draft_depth"); err != nil {
			return clientFeatures{}, err
		}
	}
	return features, nil
}

// SamplingMessage is one message of a server's sampling request, as text.
type SamplingMessage struct {
	Role string
	Text string
}

// SamplingRequest is what a server asked the session's model (sampling/createMessage).
type SamplingRequest struct {
	Server          string
	SystemPrompt    string
	Messages        []SamplingMessage
	MaxOutputTokens int
}

// The refusals of a sampling request no run's model can answer. The text goes back to the
// server as the error of its request.
var (
	errSamplingUnattributed = errors.New("this client cannot sample this request: it samples only for an input request in the result of a run's tool call, resource read or prompt get (protocol 2026-07-28); a sampling/createMessage the server sends on its own names no request of the client, so it is refused")
	errSamplingWithoutCall  = errors.New("this client cannot sample right now: it samples only for a run's tool, resource or prompt call in progress on this connection, and none is")
	errSamplingWithoutModel = errors.New("this client cannot sample right now: the call in progress has no model to sample with")
)

// errElicitationUnattributed refuses a question no request of the client asked for; the text goes
// back to the server as the error of its request.
var errElicitationUnattributed = errors.New("this client cannot show this question: it asks a run's user only for an input request in the result of that run's tool call, resource read or prompt get (protocol 2026-07-28); an elicitation/create the server sends on its own names no request of the client, so it is refused")

// Sampler answers a sampling request with the session's model: the text and the model
// that wrote it.
type Sampler func(ctx context.Context, request SamplingRequest) (text, model string, err error)

// SetSampler sets the function that answers the attributed sampling requests (sample) of
// servers whose file enables sampling, unless the call the request belongs to came from a listing
// with a sampler of its own (LoadToolsSampled); without either, such a request is refused. It
// never answers a request a server sends on its own.
func (m *Manager) SetSampler(sampler Sampler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sampler = sampler
}

// SetLogger sets the log that server_log messages go to.
func (m *Manager) SetLogger(logger *slog.Logger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logger = logger
}

func (m *Manager) currentSampler() Sampler {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sampler
}

func (m *Manager) currentLogger() *slog.Logger {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logger
}

// activeCall is the run's request (a tool call, or a resource or prompt tool's request) a
// connection is running; a server's input request in its result (attributedCall) asks that call's
// user for an elicitation, and is answered by sampler for a sampling when the tool was listed with
// one (LoadToolsSampled).
type activeCall struct {
	request  tools.InteractionHandler
	identity tools.InteractionIdentity
	sampler  Sampler
}

// clientState is shared by a connection and the handlers of its client, which exist
// before the connection does.
type clientState struct {
	mu        sync.Mutex
	samplings int
}

// inlineRequest marks the context of a request the client sends (callTool, request): the run's
// call it serves, nil for one no run made (the user's resource and prompt requests, a resubscribe).
type inlineRequest struct{ call *activeCall }

type inlineRequestKey struct{}

func withInlineRequest(ctx context.Context, call *activeCall) context.Context {
	return context.WithValue(ctx, inlineRequestKey{}, inlineRequest{call: call})
}

// attributedCall is the request of ours a server's sampling or elicitation request answers, when
// the request provably belongs to it: on protocol 2026-07-28 a server asks through input requests in the
// result of a call, resource read or prompt get (multi round-trip), and the SDK's client fulfils
// them with the context of the request that got that result, which carries its inlineRequest. A
// sampling/createMessage or elicitation/create the server sends on its own (older protocols, or a
// server ignoring that rule; over stdio and HTTP alike) reaches the handler with the connection's context instead,
// which carries none, whatever call is in progress: ok is false. The mark lives only in the
// contexts of the client's own requests, which no server can set.
func attributedCall(ctx context.Context) (call *activeCall, ok bool) {
	request, ok := ctx.Value(inlineRequestKey{}).(inlineRequest)
	return request.call, ok
}

// workspaceRoot is the file URI of the session's working directory.
func workspaceRoot(cwd string) *mcpSDK.Root {
	absolute, err := filepath.Abs(cwd)
	if err != nil || cwd == "" {
		return nil
	}
	location := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	return &mcpSDK.Root{URI: location.String(), Name: filepath.Base(absolute)}
}

// featureClientOptions adds the handlers of features to options.
func (m *Manager) featureClientOptions(options *mcpSDK.ClientOptions, serverID string, features clientFeatures, state *clientState, eventsMax int) {
	options.ToolListChangedHandler = func(context.Context, *mcpSDK.ToolListChangedRequest) {
		m.events.record(serverID, ResourceEventToolsChanged, "", eventsMax)
	}
	options.PromptListChangedHandler = func(context.Context, *mcpSDK.PromptListChangedRequest) {
		m.events.record(serverID, ResourceEventPromptsChanged, "", eventsMax)
	}
	if features.Roots == RootsNone {
		// No roots capability at all, rather than an empty list.
		options.Capabilities = &mcpSDK.ClientCapabilities{}
	}
	if level, logging := serverLogLevels[features.ServerLog]; logging {
		options.LoggingMessageHandler = func(_ context.Context, request *mcpSDK.LoggingMessageRequest) {
			if request == nil || request.Params == nil {
				return
			}
			messageLevel := mcpLogLevel(string(request.Params.Level))
			logger := m.currentLogger()
			if logger == nil || messageLevel < level {
				return
			}
			data, _ := json.Marshal(request.Params.Data)
			text := cutText(string(data), features.ServerLogMaxBytes)
			logger.Log(context.Background(), messageLevel, "mcp server log", slog.String("server", serverID), slog.String("logger", request.Params.Logger), slog.String("data", text))
		}
		options.ProgressNotificationHandler = func(_ context.Context, request *mcpSDK.ProgressNotificationClientRequest) {
			logger := m.currentLogger()
			if logger == nil || request == nil || request.Params == nil || level > slog.LevelDebug {
				return
			}
			message := cutText(request.Params.Message, features.ServerLogMaxBytes)
			logger.Debug("mcp server progress", slog.String("server", serverID), slog.Float64("progress", request.Params.Progress), slog.Float64("total", request.Params.Total), slog.String("message", message))
		}
	}
	if features.Sampling {
		options.CreateMessageHandler = func(ctx context.Context, request *mcpSDK.CreateMessageRequest) (*mcpSDK.CreateMessageResult, error) {
			return m.sample(ctx, serverID, features, state, request)
		}
	}
	if features.Elicitation {
		options.ElicitationHandler = func(ctx context.Context, request *mcpSDK.ElicitRequest) (*mcpSDK.ElicitResult, error) {
			return elicit(ctx, serverID, features, request)
		}
	}
}

// sample answers one sampling request with the session's model, within the server's
// request budget per connection and its output token cap. Only text is passed on. It answers
// only a request attributed to a request of ours (attributedCall); one the server sent on its own
// is refused whatever call is in progress, since nothing in it says which run's model and
// credential it may use. The model is the sampler of the run's call the request belongs to when
// its tool was listed with one, else the manager's (an isolated run's own servers). An attributed
// request with neither is refused too: no run made the request (the user's resource or prompt
// request), or a call of a listing with no sampler (an agent toolset's servers). A refused request
// does not count against the budget and reaches no model.
func (m *Manager) sample(ctx context.Context, serverID string, features clientFeatures, state *clientState, request *mcpSDK.CreateMessageRequest) (*mcpSDK.CreateMessageResult, error) {
	call, attributed := attributedCall(ctx)
	if !attributed {
		return nil, errSamplingUnattributed
	}
	sampler := m.currentSampler()
	if call != nil && call.sampler != nil {
		sampler = call.sampler
	}
	if sampler == nil {
		if call == nil {
			return nil, errSamplingWithoutCall
		}
		return nil, errSamplingWithoutModel
	}
	if request == nil || request.Params == nil || len(request.Params.Messages) == 0 {
		return nil, errors.New("sampling/createMessage needs messages")
	}
	messages := make([]SamplingMessage, 0, len(request.Params.Messages))
	for _, message := range request.Params.Messages {
		if message == nil {
			continue
		}
		text, ok := message.Content.(*mcpSDK.TextContent)
		if !ok {
			return nil, errors.New("this client samples text messages only")
		}
		messages = append(messages, SamplingMessage{Role: string(message.Role), Text: text.Text})
	}
	state.mu.Lock()
	if state.samplings >= features.SamplingMaxCalls {
		state.mu.Unlock()
		return nil, fmt.Errorf("the client allows at most %d sampling requests per connection (sampling.max_requests)", features.SamplingMaxCalls)
	}
	state.samplings++
	state.mu.Unlock()
	sampling := SamplingRequest{Server: serverID, SystemPrompt: request.Params.SystemPrompt, Messages: messages, MaxOutputTokens: features.SamplingMaxTokens}
	if requested := int(request.Params.MaxTokens); requested > 0 && requested < sampling.MaxOutputTokens {
		sampling.MaxOutputTokens = requested
	}
	answer, model, err := sampler(ctx, sampling)
	if err != nil {
		return nil, fmt.Errorf("the client's model could not answer: %w", err)
	}
	return &mcpSDK.CreateMessageResult{Content: &mcpSDK.TextContent{Text: answer}, Model: model, Role: "assistant", StopReason: "endTurn"}, nil
}

// elicitationDraft is what the user sees and edits for a server's elicitation request.
type elicitationDraft struct {
	Server  string         `json:"server"`
	Message string         `json:"message"`
	Content map[string]any `json:"content"`
}

// elicit asks the user of the run's call the request belongs to. Like sample, it answers only a
// request attributed to a request of ours (attributedCall): one the server sent on its own is
// refused with an error whatever call is in progress, since nothing in it says which run's user
// may see it. Approving the draft accepts with its content, declining declines, and anything else
// (an attributed request no run made, no way to ask, a timeout) cancels.
func elicit(ctx context.Context, serverID string, features clientFeatures, request *mcpSDK.ElicitRequest) (*mcpSDK.ElicitResult, error) {
	call, attributed := attributedCall(ctx)
	if !attributed {
		return nil, errElicitationUnattributed
	}
	if call == nil || call.request == nil || request == nil || request.Params == nil {
		return &mcpSDK.ElicitResult{Action: "cancel"}, nil
	}
	if request.Params.Mode == "url" {
		// A URL to open is not something a draft can show safely.
		return &mcpSDK.ElicitResult{Action: "decline"}, nil
	}
	draft := elicitationDraft{Server: serverID, Message: request.Params.Message, Content: schemaDefaults(request.Params.RequestedSchema)}
	encoded, err := json.Marshal(draft)
	if err != nil {
		return &mcpSDK.ElicitResult{Action: "cancel"}, nil
	}
	answer, err := call.request(ctx, call.identity, tools.InteractionQuestion{
		Draft: encoded, Title: "MCP server " + serverID + " asks: " + firstLine(request.Params.Message),
		TimeoutMS: features.ElicitTimeoutMS, MaxTimeoutMS: features.ElicitTimeoutMS,
		Limits: tools.InteractionLimits{MaxDraftBytes: features.ElicitDraftBytes, MaxDraftDepth: features.ElicitDraftDepth},
	})
	if err != nil {
		return &mcpSDK.ElicitResult{Action: "cancel"}, nil
	}
	if answer.Outcome != tools.InteractionAnswered {
		return &mcpSDK.ElicitResult{Action: "cancel"}, nil
	}
	switch answer.Decision {
	case tools.InteractionAccept, tools.InteractionUpdate:
		edited := draft
		if len(answer.Draft) > 0 {
			if json.Unmarshal(answer.Draft, &edited) != nil {
				return &mcpSDK.ElicitResult{Action: "cancel"}, nil
			}
		}
		return &mcpSDK.ElicitResult{Action: "accept", Content: edited.Content}, nil
	case tools.InteractionDecline:
		return &mcpSDK.ElicitResult{Action: "decline"}, nil
	}
	return &mcpSDK.ElicitResult{Action: "cancel"}, nil
}

// schemaDefaults is an object with each property of a requested schema set to its
// default, or to the zero value of its type.
func schemaDefaults(schema any) map[string]any {
	content := map[string]any{}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return content
	}
	var decoded struct {
		Properties map[string]struct {
			Type    string `json:"type"`
			Default any    `json:"default"`
		} `json:"properties"`
	}
	if json.Unmarshal(encoded, &decoded) != nil {
		return content
	}
	for name, property := range decoded.Properties {
		switch {
		case property.Default != nil:
			content[name] = property.Default
		case property.Type == "boolean":
			content[name] = false
		case property.Type == "number" || property.Type == "integer":
			content[name] = 0
		default:
			content[name] = ""
		}
	}
	return content
}

func mcpLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info", "notice":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return cutText(line, maxElicitationTitleBytes)
}

type activeCallKey struct{}

func withActiveCall(ctx context.Context, call *activeCall) context.Context {
	return context.WithValue(ctx, activeCallKey{}, call)
}

func activeCallFrom(ctx context.Context) *activeCall {
	call, _ := ctx.Value(activeCallKey{}).(*activeCall)
	return call
}

// setServerLogLevel asks a server with server_log set to send its log messages from that
// level up. A server without the logging capability refuses, which changes nothing.
func (m *Manager) setServerLogLevel(ctx context.Context, current *connection, features clientFeatures) {
	if _, logging := serverLogLevels[features.ServerLog]; !logging {
		return
	}
	_ = current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
		return session.SetLoggingLevel(ctx, &mcpSDK.SetLoggingLevelParams{Level: mcpSDK.LoggingLevel(features.ServerLog)})
	})
}
