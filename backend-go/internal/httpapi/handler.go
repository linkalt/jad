package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/memorytrace"
	"jad/backend-go/internal/serverconfig"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessiongate"
)

const (
	minAPITokenLength        = 32
	maxSafeJSONInteger int64 = 9007199254740991
)

var (
	sessionRootIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sessionReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$|^[A-Za-z0-9][A-Za-z0-9._-]{0,63}:[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// A profile is a legacy name (letters, digits, '_' and '-') or, in the catalog layout,
	// <category>/<name>, the same identifier the catalog package accepts.
	profileNamePattern           = regexp.MustCompile(`^[A-Za-z0-9_-]+$|^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	decimalIntegerPattern        = regexp.MustCompile(`^\d+$`)
	cursorGenerationPattern      = regexp.MustCompile(`^\d+:\d+:\d+$`)
	frontCompactMessageIDPattern = regexp.MustCompile(`^session-user-o(0|[1-9][0-9]*)$`)
)

// Config contains the externally loaded HTTP/SSE settings. Limits and origins
// are required here rather than silently receiving server-local defaults.
type Config struct {
	// Logger receives the handler's diagnostics. Nil discards them.
	Logger              *slog.Logger
	APIToken            string
	AllowedOrigins      []string
	MaxBodyBytes        int64
	SSEMaxBufferedBytes int
	// SSEHeartbeatInterval is the gap between keep-alive comment frames on a
	// run stream with nothing else to send. Zero disables them.
	SSEHeartbeatInterval time.Duration
	// SSEWriteTimeout is how long one write to a run stream may take before the
	// subscriber is dropped (server.sse_write_timeout_ms). Zero disables the limit.
	SSEWriteTimeout time.Duration
	// ShutdownTimeout bounds how long Close waits for cancelled runs to end
	// (server.shutdown_timeout_ms). Zero waits without a limit.
	ShutdownTimeout time.Duration
	// SessionDeleteStopWait is how long a session delete waits for the session's stopped runs.
	SessionDeleteStopWait      time.Duration
	RunStateRetention          time.Duration
	AllowZeroRunStateRetention bool
	ToolStreamInterval         time.Duration
	ConditionalRequests        bool
	SnapshotTurns              int
	MaxPageTurns               int
	MaxInlinePartBytes         int
	// SteerMaxPending is the most steering messages one run holds (server.steer_max_pending);
	// zero turns POST /run/steer off.
	SteerMaxPending int
	// ForkNameMaxBytes is the longest name and display name a fork into a new profile may
	// give it (server.fork_name_max_bytes); zero uses serverconfig.DefaultForkNameMaxBytes.
	ForkNameMaxBytes int
	// MaxRequestImages and MaxRequestImageBytes bound the images a run request may
	// carry (server.max_request_images, server.max_request_image_bytes). Zero images
	// turns image input off.
	MaxRequestImages     int
	MaxRequestImageBytes int64
	// MaxRequestFiles and MaxRequestFileBytes bound the files (PDF) a run request may
	// carry (server.max_request_files, server.max_request_file_bytes). Zero files turns
	// file input off.
	MaxRequestFiles     int
	MaxRequestFileBytes int64
	// MaxConcurrentRuns is the most runs running at once (server.max_concurrent_runs);
	// a start beyond it is refused with 429. Zero means no cap.
	MaxConcurrentRuns     int
	RunContentMemoryBytes int
	// RunContentFallbackMaxBytes caps the in-memory copy of the streamed text kept when the
	// partial file is unusable. Zero means serverconfig.DefaultRunContentFallbackMaxBytes.
	RunContentFallbackMaxBytes int
	RunContentWriteBatchBytes  int
	RunContentFlushInterval    time.Duration
	// MaxIndexSegments is accepted for root-config parity; the current Go
	// output_text cache merges into one part and does not need a range index.
	RunContentMaxIndexSegments int
	MemoryTrace                *memorytrace.Trace
}

// StatusError lets a resolver return a safe, intentional HTTP error without
// coupling its implementation to this package's routing logic.
type StatusError struct {
	Code    int
	Message string
	// RetryAfterSeconds, when positive, is sent as the Retry-After header.
	RetryAfterSeconds int
}

func (e *StatusError) Error() string { return e.Message }

// Handler implements the health and session-run HTTP/SSE routes.
type Handler struct {
	logger                    *slog.Logger
	resolver                  SessionResolver
	catalog                   SessionCatalog
	toolLister                SessionToolLister
	mcpPrompts                SessionMCPPrompts
	mcpResources              SessionMCPResources
	settings                  SessionSettings
	catalogFiles              CatalogFiles
	catalogSessions           CatalogSessions
	images                    SessionImages
	messageImages             MessageImages
	maintenance               SessionMaintenance
	library                   RootLibrary
	tokenHash                 [sha256.Size]byte
	allowedOrigin             map[string]struct{}
	maxBodyBytes              int64
	runInputLimits            RunInputLimits
	toolStreamInterval        time.Duration
	sseHeartbeatInterval      time.Duration
	sessionDeleteStopWait     time.Duration
	conditionalRequests       bool
	snapshotTurns             int
	maxPageTurns              int
	maxInlinePartBytes        int
	memoryTrace               *memorytrace.Trace
	runContentMemoryBytes     int
	runContentFallbackMax     int
	sseWriteTimeout           time.Duration
	shutdownTimeout           time.Duration
	runContentWriteBatchBytes int
	runContentFlushInterval   time.Duration
	runs                      *runRegistry
	gate                      *sessiongate.Gate
	forkNameMaxBytes          int
	startTurns                turnQueue
}

// NewHandler validates configuration and builds an HTTP handler. The resolver
// owns session/config loading; the handler owns run retention and SSE fan-out.
func NewHandler(config Config, resolver SessionResolver) (*Handler, error) {
	return newHandler(config, resolver, nil)
}

// NewHandlerWithRootLibrary adds the optional read-only root-library routes.
func NewHandlerWithRootLibrary(config Config, resolver SessionResolver, library RootLibrary) (*Handler, error) {
	if library == nil {
		return nil, errors.New("root library provider is required")
	}
	return newHandler(config, resolver, library)
}

func newHandler(config Config, resolver SessionResolver, library RootLibrary) (*Handler, error) {
	if resolver == nil {
		return nil, errors.New("session resolver is required")
	}
	if len(config.APIToken) < minAPITokenLength || strings.ContainsAny(config.APIToken, "\r\n") {
		return nil, fmt.Errorf("API token must be at least %d bytes and contain no line breaks", minAPITokenLength)
	}
	if config.MaxBodyBytes <= 0 {
		return nil, errors.New("maximum request body bytes must be positive")
	}
	if config.SSEMaxBufferedBytes < 1024 {
		return nil, errors.New("maximum SSE buffered bytes must be at least 1024")
	}
	if config.SessionDeleteStopWait < 0 || config.SessionDeleteStopWait > time.Duration(serverconfig.MaxSessionDeleteStopWaitMS)*time.Millisecond {
		return nil, fmt.Errorf("session delete stop wait must be between 0 and %d milliseconds", serverconfig.MaxSessionDeleteStopWaitMS)
	}
	if config.SSEHeartbeatInterval < 0 || config.SSEHeartbeatInterval > time.Duration(serverconfig.MaxSSEHeartbeatIntervalMS)*time.Millisecond {
		return nil, fmt.Errorf("SSE heartbeat interval must be between 0 and %d milliseconds", serverconfig.MaxSSEHeartbeatIntervalMS)
	}
	if config.RunStateRetention < 0 || (config.RunStateRetention == 0 && !config.AllowZeroRunStateRetention) {
		return nil, errors.New("run state retention must be positive unless zero retention is explicitly configured")
	}
	if config.ToolStreamInterval < 0 || config.ToolStreamInterval > time.Duration(serverconfig.MaxToolStreamIntervalMS)*time.Millisecond {
		return nil, fmt.Errorf("tool stream interval must be between 0 and %d milliseconds", serverconfig.MaxToolStreamIntervalMS)
	}
	if config.SnapshotTurns < 0 {
		return nil, errors.New("snapshot turns must not be negative")
	}
	if config.SteerMaxPending < 0 || config.SteerMaxPending > serverconfig.MaxSteerMaxPending {
		return nil, fmt.Errorf("steer max pending must be between 0 and %d", serverconfig.MaxSteerMaxPending)
	}
	if config.ForkNameMaxBytes < 0 || config.ForkNameMaxBytes > serverconfig.MaxForkNameMaxBytes {
		return nil, fmt.Errorf("fork name max bytes must be between 1 and %d", serverconfig.MaxForkNameMaxBytes)
	}
	if config.ForkNameMaxBytes == 0 {
		config.ForkNameMaxBytes = serverconfig.DefaultForkNameMaxBytes
	}
	if config.MaxPageTurns < 1 {
		return nil, errors.New("maximum page turns must be positive")
	}
	if config.MaxInlinePartBytes < 0 {
		return nil, errors.New("maximum inline part bytes must not be negative")
	}
	if config.MaxRequestImages < 0 || config.MaxRequestImageBytes < 0 || (config.MaxRequestImages > 0 && config.MaxRequestImageBytes < 1) {
		return nil, errors.New("request image limits must not be negative, and an image limit needs a byte limit")
	}
	if config.MaxRequestFiles < 0 || config.MaxRequestFileBytes < 0 || (config.MaxRequestFiles > 0 && config.MaxRequestFileBytes < 1) {
		return nil, errors.New("request file limits must not be negative, and a file limit needs a byte limit")
	}
	if config.MaxConcurrentRuns < 0 || config.MaxConcurrentRuns > serverconfig.MaxMaxConcurrentRuns {
		return nil, fmt.Errorf("maximum concurrent runs must be between 0 and %d", serverconfig.MaxMaxConcurrentRuns)
	}
	if config.RunContentMemoryBytes < 0 || config.RunContentMemoryBytes > serverconfig.MaxRunContentMemoryBytes {
		return nil, errors.New("run content memory bytes must be between 0 and 16777216")
	}
	if config.RunContentFallbackMaxBytes == 0 {
		config.RunContentFallbackMaxBytes = serverconfig.DefaultRunContentFallbackMaxBytes
	}
	if config.RunContentFallbackMaxBytes < serverconfig.MinRunContentFallbackMaxBytes || config.RunContentFallbackMaxBytes > serverconfig.MaxRunContentFallbackMaxBytes {
		return nil, fmt.Errorf("run content fallback max bytes must be between %d and %d", serverconfig.MinRunContentFallbackMaxBytes, serverconfig.MaxRunContentFallbackMaxBytes)
	}
	if config.SSEWriteTimeout < 0 || config.SSEWriteTimeout > time.Duration(serverconfig.MaxSSEWriteTimeoutMS)*time.Millisecond {
		return nil, fmt.Errorf("SSE write timeout must be between 0 and %d milliseconds", serverconfig.MaxSSEWriteTimeoutMS)
	}
	if config.RunContentWriteBatchBytes == 0 {
		config.RunContentWriteBatchBytes = serverconfig.DefaultRunContentWriteBatchBytes
	}
	if config.RunContentWriteBatchBytes < 2 || config.RunContentWriteBatchBytes > serverconfig.MaxRunContentMemoryBytes || config.RunContentWriteBatchBytes%2 != 0 {
		return nil, errors.New("run content write batch bytes must be an even integer between 2 and 16777216")
	}
	if config.RunContentFlushInterval < 0 || config.RunContentFlushInterval > time.Duration(serverconfig.MaxRunContentFlushIntervalMS)*time.Millisecond {
		return nil, errors.New("run content flush interval must be between 0 and 60000 milliseconds")
	}
	if config.RunContentMaxIndexSegments == 0 {
		config.RunContentMaxIndexSegments = serverconfig.DefaultRunContentMaxIndexSegments
	}
	if config.RunContentMaxIndexSegments < 1 || config.RunContentMaxIndexSegments > serverconfig.MaxRunContentIndexSegments {
		return nil, errors.New("run content max index segments must be between 1 and 65536")
	}

	origins := make(map[string]struct{}, len(config.AllowedOrigins))
	for _, origin := range config.AllowedOrigins {
		if origin == "" {
			return nil, errors.New("allowed origins must not contain empty values")
		}
		origins[origin] = struct{}{}
	}
	catalog, _ := resolver.(SessionCatalog)
	toolLister, _ := resolver.(SessionToolLister)
	mcpPrompts, _ := resolver.(SessionMCPPrompts)
	mcpResources, _ := resolver.(SessionMCPResources)
	settings, _ := resolver.(SessionSettings)
	catalogFiles, _ := resolver.(CatalogFiles)
	catalogSessions, _ := resolver.(CatalogSessions)
	images, _ := resolver.(SessionImages)
	messageImages, _ := resolver.(MessageImages)
	maintenance, _ := resolver.(SessionMaintenance)
	gate := newSessionGate(resolver)
	trace := config.MemoryTrace
	if trace == nil {
		trace = memorytrace.FromEnvironment()
	}
	return &Handler{
		logger:                    logging.Or(config.Logger),
		resolver:                  resolver,
		images:                    images,
		messageImages:             messageImages,
		catalog:                   catalog,
		toolLister:                toolLister,
		mcpPrompts:                mcpPrompts,
		mcpResources:              mcpResources,
		settings:                  settings,
		catalogFiles:              catalogFiles,
		catalogSessions:           catalogSessions,
		maintenance:               maintenance,
		library:                   library,
		tokenHash:                 sha256.Sum256([]byte(config.APIToken)),
		allowedOrigin:             origins,
		maxBodyBytes:              config.MaxBodyBytes,
		runInputLimits:            RunInputLimits{MaxImages: config.MaxRequestImages, MaxImageBytes: config.MaxRequestImageBytes, MaxFiles: config.MaxRequestFiles, MaxFileBytes: config.MaxRequestFileBytes},
		toolStreamInterval:        config.ToolStreamInterval,
		sseHeartbeatInterval:      config.SSEHeartbeatInterval,
		sessionDeleteStopWait:     config.SessionDeleteStopWait,
		conditionalRequests:       config.ConditionalRequests,
		snapshotTurns:             config.SnapshotTurns,
		maxPageTurns:              config.MaxPageTurns,
		maxInlinePartBytes:        config.MaxInlinePartBytes,
		memoryTrace:               trace,
		runContentMemoryBytes:     config.RunContentMemoryBytes,
		runContentFallbackMax:     config.RunContentFallbackMaxBytes,
		sseWriteTimeout:           config.SSEWriteTimeout,
		shutdownTimeout:           config.ShutdownTimeout,
		runContentWriteBatchBytes: config.RunContentWriteBatchBytes,
		runContentFlushInterval:   config.RunContentFlushInterval,
		runs:                      newRunRegistry(config.RunStateRetention, config.SSEMaxBufferedBytes, config.MaxConcurrentRuns).withSteerLimit(config.SteerMaxPending),
		gate:                      gate,
		forkNameMaxBytes:          config.ForkNameMaxBytes,
	}, nil
}

// Close cancels active session runs and waits for them to end, at most the shutdown timeout.
// Call it during server shutdown.
func (h *Handler) Close() {
	if h != nil && h.runs != nil {
		h.runs.close(h.shutdownTimeout)
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
		return
	}
	h.observeRequest(w, r, func(w http.ResponseWriter) { h.route(w, r) })
}

// route dispatches one request to its handler.
func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	if !h.applyCORS(w, r) {
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	segments, err := decodedPathSegments(r.URL)
	if err != nil {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "invalid URL encoding"})
		return
	}
	if r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/health" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "library" {
		if !h.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="jad"`)
			writeJSONError(w, &StatusError{Code: http.StatusUnauthorized, Message: "API token required"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h.handleRootLibrary(w, r, segments)
		return
	}
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "catalog" {
		if !h.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="jad"`)
			writeJSONError(w, &StatusError{Code: http.StatusUnauthorized, Message: "API token required"})
			return
		}
		h.handleCatalogFiles(w, r, segments)
		return
	}
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "server" {
		if !h.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="jad"`)
			writeJSONError(w, &StatusError{Code: http.StatusUnauthorized, Message: "API token required"})
			return
		}
		h.handleServerInfo(w, r)
		return
	}
	if len(segments) < 2 || segments[0] != "api" || segments[1] != "sessions" {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "not found"})
		return
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="jad"`)
		writeJSONError(w, &StatusError{Code: http.StatusUnauthorized, Message: "API token required"})
		return
	}

	if len(segments) == 2 && r.Method == http.MethodGet {
		h.handleSessionList(w, r)
		return
	}
	if len(segments) < 3 {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "not found"})
		return
	}
	reference := segments[2]
	if message := invalidSessionReferenceMessage(reference); message != "" {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: message})
		return
	}
	profile, err := profileParameter(r.URL)
	if err != nil {
		writeJSONError(w, err)
		return
	}

	switch {
	case r.Method == http.MethodGet && len(segments) == 3:
		h.handleSessionSnapshot(w, r, reference, profile)
	case r.Method == http.MethodGet && len(segments) == 4 && segments[3] == "tools":
		h.handleSessionTools(w, r, reference, profile)
	case r.Method == http.MethodGet && len(segments) == 4 && segments[3] == "history":
		h.handleSessionHistory(w, r, reference, profile)
	case r.Method == http.MethodGet && len(segments) == 5 && segments[3] == "messages":
		h.handleSessionMessage(w, r, reference, segments[4], profile)
	case r.Method == http.MethodGet && len(segments) == 8 && segments[3] == "messages" && segments[5] == "images":
		h.handleMessageImage(w, r, reference, profile, segments[4], segments[6], segments[7])
	case r.Method == http.MethodGet && len(segments) == 5 && segments[3] == "images":
		h.handleSessionImage(w, r, reference, profile, segments[4])
	case r.Method == http.MethodGet && len(segments) == 4 && segments[3] == "mcp-prompts":
		h.handleMCPPromptList(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 5 && segments[3] == "mcp-prompts" && segments[4] == "get":
		h.handleMCPPromptGet(w, r, reference, profile)
	case r.Method == http.MethodGet && len(segments) == 4 && segments[3] == "mcp-resources":
		h.handleMCPResourceList(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 5 && segments[3] == "mcp-resources" && segments[4] == "read":
		h.handleMCPResourceRead(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 5 && segments[3] == "mcp-resources" && (segments[4] == "subscribe" || segments[4] == "unsubscribe"):
		h.handleMCPResourceSubscribe(w, r, reference, profile, segments[4] == "subscribe")
	case r.Method == http.MethodGet && len(segments) == 5 && segments[3] == "mcp-resources" && segments[4] == "events":
		h.handleMCPResourceEvents(w, r, reference, profile)
	case r.Method == http.MethodGet && len(segments) == 4 && segments[3] == "files":
		h.handleSessionFiles(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "model-default":
		h.handleSessionModelDefault(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "compact":
		h.handleSessionCompact(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "reset":
		h.handleSessionReset(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "fork":
		h.handleSessionFork(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "front-compact":
		h.handleSessionFrontCompact(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "statecontext":
		h.handleSessionStateContext(w, r, reference, profile)
	case r.Method == http.MethodGet && len(segments) == 4 && segments[3] == "checkpoints":
		h.handleCheckpointList(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 6 && segments[3] == "checkpoints" && segments[5] == "restore":
		h.handleCheckpointRestore(w, r, reference, profile, segments[4])
	case r.Method == http.MethodPost && len(segments) == 6 && segments[3] == "run" && segments[4] == "interaction" && segments[5] == "extend":
		h.handleRunInteractionExtend(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 5 && segments[3] == "run" && segments[4] == "interaction":
		h.handleRunInteraction(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "run":
		h.handleRunStart(w, r, reference, profile)
	case r.Method == http.MethodGet && len(segments) == 5 && segments[3] == "run" && segments[4] == "stream":
		h.handleRunStream(w, r, reference, profile)
	case r.Method == http.MethodGet && len(segments) == 7 && segments[3] == "run" && segments[4] == "images":
		h.handleLiveRunImage(w, r, reference, profile, segments[5], segments[6])
	case r.Method == http.MethodPost && len(segments) == 5 && segments[3] == "run" && segments[4] == "stop":
		h.handleRunStop(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 5 && segments[3] == "run" && segments[4] == "steer":
		h.handleRunSteer(w, r, reference, profile)
	default:
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "not found"})
	}
}

func (h *Handler) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if _, allowed := h.allowedOrigin[origin]; !allowed {
		writeJSONError(w, &StatusError{Code: http.StatusForbidden, Message: "origin is not allowed"})
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Add("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match, If-None-Match")
	w.Header().Set("Access-Control-Expose-Headers", "ETag")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	return true
}

func (h *Handler) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	candidate := sha256.Sum256([]byte(strings.TrimPrefix(header, prefix)))
	return subtle.ConstantTimeCompare(candidate[:], h.tokenHash[:]) == 1
}

func (h *Handler) handleRunStart(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if _, ok := w.(http.Flusher); !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming responses are unavailable"})
		return
	}
	body, err := readRunBody(w, r, h.runBodyLimit(), h.maxBodyBytes, h.runInputLimits)
	if err != nil {
		writeJSONError(w, err)
		return
	}

	// The transcript lock is only tried while the session lock is held. When another run
	// holds it, the wait happens outside the session lock, so settings, state context and
	// the other profile's start are not blocked behind a queued start. Taking the lock and
	// registering the run stay in one session-lock section: that is what lets reset, front
	// compact, state context and checkpoint restore trust runIsActive.
	started, err := h.acquireRun(r.Context(), reference, profile, body.Model, "", "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	run, resolved, processUnlock := started.run, started.resolved, started.processUnlock
	launched := false
	defer func() {
		if !launched {
			run.finish("failed", failureEvents("The run failed before it started."))
			processUnlock()
		}
	}()
	run.mu.Lock()
	run.supportsInteraction = body.SupportsInteraction
	run.mu.Unlock()
	run.outputFormat = body.OutputFormat
	run.outputValidator = outputFormatValidator(body.OutputFormat)
	run.plan, run.tools, run.executePlan = body.Plan, body.Tools, body.ExecutePlan
	h.attachContentCache(run)
	queue := run.subscribe()
	launched = true
	go func() {
		defer processUnlock()
		h.executeRun(run, resolved.Runner, body.Input)
	}()
	h.serveSSE(w, r, run, queue)
}

func (h *Handler) attachContentCache(run *activeRun) {
	contentCache := newRunContentCache(h.runContentMemoryBytes, h.runContentWriteBatchBytes, h.runContentFlushInterval)
	contentCache.fallbackMax = h.runContentFallbackMax
	contentCache.logger = h.logger.With(runLogAttrs(run)...)
	run.setContentCache(contentCache)
}

// startedRun is a run that holds the transcript lock and is registered.
type startedRun struct {
	resolved      ResolvedSession
	run           *activeRun
	processUnlock func()
}

// tryStartRun takes the session lock, tries the transcript lock, resolves the session and
// registers the run, all inside one session-lock section. busy is true when another run
// holds the transcript; nothing is held then.
func (h *Handler) tryStartRun(ctx context.Context, reference, profile, model, source, caller string) (started startedRun, busy bool, err error) {
	unlock := h.lockSession(reference)
	defer unlock()
	processUnlock, err := h.tryLockSessionProcess(ctx, reference, profile, model)
	if errors.Is(err, ErrSessionBusy) {
		return startedRun{}, true, nil
	}
	if err != nil {
		return startedRun{}, false, err
	}
	handedOver := false
	defer func() {
		if !handedOver {
			processUnlock()
		}
	}()
	resolved, err := h.resolve(ctx, reference, profile, model)
	if err != nil {
		return startedRun{}, false, err
	}
	turnID, err := newWebTurnID()
	if err != nil {
		return startedRun{}, false, &StatusError{Code: http.StatusInternalServerError, Message: "could not create run ID"}
	}
	run, err := h.beginRunFrom(h.runKey(resolved), turnID, source, caller)
	if err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) {
			return startedRun{}, false, statusErr
		}
		return startedRun{}, false, &StatusError{Code: http.StatusServiceUnavailable, Message: "server is shutting down"}
	}
	handedOver = true
	return startedRun{resolved: resolved, run: run, processUnlock: processUnlock}, false, nil
}

func (h *Handler) handleRunStream(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if _, ok := w.(http.Flusher); !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming responses are unavailable"})
		return
	}
	resolved, err := h.resolve(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	run := h.runs.current(h.runKey(resolved))
	if run == nil {
		h.writeSSEHeaders(w)
		idle, _ := makeSSEEvent("idle", struct{}{})
		_, _ = w.Write(idle.frame)
		w.(http.Flusher).Flush()
		return
	}
	h.serveSSE(w, r, run, run.subscribe())
}

func (h *Handler) handleRunStop(w http.ResponseWriter, r *http.Request, reference, profile string) {
	resolved, err := h.resolve(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	stop := h.runs.stop(h.runKey(resolved))
	// A child handed off earlier is not part of the run, so stopping the run does not
	// reach it. The user asked everything of this session to stop, whether or not a run
	// was still going.
	h.cancelSessionChildren(resolved.Reference, resolved.Profile)
	writeJSON(w, http.StatusOK, map[string]any{"stop": stop})
}

func (h *Handler) handleSessionTools(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.toolLister == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session tool listing is unavailable"})
		return
	}
	listed, err := h.toolLister.ListTools(r.Context(), reference, profile)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listed)
}

func (h *Handler) handleSessionList(w http.ResponseWriter, r *http.Request) {
	if h.catalog == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session listing is unavailable"})
		return
	}
	result, err := h.catalog.ListSessions(r.Context(), h.conditionalETag(r), h.runs)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	h.writeCatalogResponse(w, r, result)
}

func (h *Handler) handleSessionSnapshot(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.catalog == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session snapshots are unavailable"})
		return
	}
	options, err := snapshotOptions(r.URL.Query())
	if err != nil {
		writeJSONError(w, err)
		return
	}
	result, err := h.catalog.ReadSessionSnapshot(r.Context(), reference, profile, options, h.conditionalETag(r), h.runs)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	h.writeCatalogResponse(w, r, result)
}

func (h *Handler) handleSessionHistory(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.catalog == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session history is unavailable"})
		return
	}
	result, err := h.catalog.ReadSessionHistory(r.Context(), reference, profile, h.conditionalETag(r))
	if err != nil {
		writeJSONError(w, err)
		return
	}
	h.writeCatalogResponse(w, r, result)
}

func (h *Handler) handleSessionMessage(w http.ResponseWriter, r *http.Request, reference, messageID, profile string) {
	if h.catalog == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session messages are unavailable"})
		return
	}
	cursor, err := parseNonNegativeQueryInteger(r.URL.Query().Get("cursor"), "cursor")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	message, err := h.catalog.ReadSessionMessage(r.Context(), reference, profile, messageID, cursor)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]json.RawMessage{"message": message})
}

func (h *Handler) writeCatalogResponse(w http.ResponseWriter, r *http.Request, result CatalogResponse) {
	// As in the TypeScript server, a versioned body is storable but must be
	// revalidated (no-cache) and carries its ETag only when conditional requests
	// are enabled; otherwise it is no-store with no validator.
	versioned := h.conditionalRequests && result.ETag != ""
	if result.NotModified {
		if !h.conditionalRequests || !ETagMatches(r.Header.Get("If-None-Match"), result.ETag) {
			writeJSONError(w, errors.New("session catalog returned an invalid not-modified response"))
			return
		}
		w.Header().Set("ETag", result.ETag)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if h.conditionalRequests && ETagMatches(r.Header.Get("If-None-Match"), result.ETag) {
		w.Header().Set("ETag", result.ETag)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if len(result.Body) == 0 || !json.Valid(result.Body) {
		writeJSONError(w, errors.New("session catalog returned invalid JSON"))
		return
	}
	if versioned {
		w.Header().Set("ETag", result.ETag)
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Body)
}

func snapshotOptions(values url.Values) (SnapshotOptions, error) {
	before, err := parseNonNegativeQueryInteger(values.Get("before"), "before")
	if err != nil {
		return SnapshotOptions{}, err
	}
	turnsValue := values.Get("turns")
	var turns *int
	if turnsValue != "" {
		parsed, err := parseNonNegativeQueryInteger(turnsValue, "turns")
		if err != nil || parsed == nil || *parsed < 1 || int64(int(*parsed)) != *parsed {
			return SnapshotOptions{}, &StatusError{Code: http.StatusBadRequest, Message: "turns must be a positive integer"}
		}
		value := int(*parsed)
		turns = &value
	}
	generation := values.Get("cursor_generation")
	if generation != "" && !cursorGenerationPattern.MatchString(generation) {
		return SnapshotOptions{}, &StatusError{Code: http.StatusBadRequest, Message: "cursor_generation must be a transcript generation"}
	}
	return SnapshotOptions{Before: before, CursorGeneration: generation, Turns: turns}, nil
}

func parseNonNegativeQueryInteger(value, name string) (*int64, error) {
	if value == "" {
		return nil, nil
	}
	if !decimalIntegerPattern.MatchString(value) {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: name + " must be a non-negative integer offset"}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed > maxSafeJSONInteger {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: name + " must be a non-negative integer offset"}
	}
	return &parsed, nil
}

func (h *Handler) conditionalETag(r *http.Request) string {
	if !h.conditionalRequests || r == nil {
		return ""
	}
	return r.Header.Get("If-None-Match")
}

// ETagMatches reports whether an If-None-Match value matches an entity tag.
// Weak comparison is correct for GET revalidation.
func ETagMatches(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func (h *Handler) resolve(ctx context.Context, reference, profile, modelPath string) (ResolvedSession, error) {
	resolved, err := h.resolver.Resolve(ctx, reference, profile, modelPath)
	if err != nil {
		return ResolvedSession{}, normalizeResolverError(err)
	}
	if !sessionReferencePattern.MatchString(resolved.Reference) || resolved.Runner == nil {
		return ResolvedSession{}, errors.New("session resolver returned an incomplete session")
	}
	if resolved.Profile != "" && !profileNamePattern.MatchString(resolved.Profile) {
		return ResolvedSession{}, errors.New("session resolver returned an invalid profile")
	}
	return resolved, nil
}

func (h *Handler) serveSSE(w http.ResponseWriter, r *http.Request, run *activeRun, queue *eventQueue) {
	defer run.detach(queue)
	flusher := w.(http.Flusher)
	// Each write gets its own deadline, so a peer that stops reading is dropped after
	// sseWriteTimeout while a healthy stream of any length is not cut. The deadline is
	// cleared on return because the connection may serve another request.
	controller := http.NewResponseController(w)
	armWriteDeadline := func() {}
	chunk := 0
	if h.sseWriteTimeout > 0 {
		armWriteDeadline = func() { _ = controller.SetWriteDeadline(time.Now().Add(h.sseWriteTimeout)) }
		chunk = sseWriteChunkBytes
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
	// writeFrame sends a frame in chunks with the deadline renewed before each, so it
	// measures a reader making no progress, not the time a large frame takes to drain.
	writeFrame := func(frame []byte) error {
		for chunk > 0 && len(frame) > chunk {
			armWriteDeadline()
			if _, err := w.Write(frame[:chunk]); err != nil {
				return err
			}
			frame = frame[chunk:]
		}
		armWriteDeadline()
		_, err := w.Write(frame)
		return err
	}
	armWriteDeadline()
	h.writeSSEHeaders(w)
	flusher.Flush()
	for {
		event, ok, quiet := queue.nextOrIdle(r.Context(), h.sseHeartbeatInterval)
		if quiet {
			// A comment frame carries no event, so a client parser ignores it; it
			// only proves the connection is alive during a long silent tool call.
			if err := writeFrame(sseKeepAliveFrame); err != nil {
				return
			}
			flusher.Flush()
			continue
		}
		if !ok {
			return
		}
		if err := writeFrame(event.frame); err != nil {
			return
		}
		if event.name == "done" {
			flusher.Flush()
			return
		}
		// Frames that queued while this one was waiting go out in the same flush: one
		// push per burst instead of one per frame (token deltas arrive in bursts).
		for _, more := range queue.takeQueued() {
			if err := writeFrame(more.frame); err != nil {
				return
			}
			if more.name == "done" {
				flusher.Flush()
				return
			}
		}
		flusher.Flush()
	}
}

// sseWriteChunkBytes is the most bytes written under one write deadline.
const sseWriteChunkBytes = 64 << 10

// sseKeepAliveFrame is the SSE comment sent on a quiet run stream.
var sseKeepAliveFrame = []byte(": keep-alive\n\n")

func (h *Handler) writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// runBodyLimit is the body limit of a run request: server.max_body_bytes plus room for
// the images and files it may carry, as base64 (4 bytes for every 3). Every other route
// keeps max_body_bytes.
func (h *Handler) runBodyLimit() int64 {
	limits := h.runInputLimits
	perImage := limits.MaxImageBytes/3*4 + 8
	perFile := limits.MaxFileBytes/3*4 + 8
	return h.maxBodyBytes + int64(limits.MaxImages)*perImage + int64(limits.MaxFiles)*perFile
}

func (h *Handler) runKey(resolved ResolvedSession) string {
	return resolved.Reference + "\x00" + resolved.Profile
}

type runRequest struct {
	// Input is what the runner takes: a JSON string, or a user message item when the
	// request sent an array of parts (see parseRunInput).
	Input               json.RawMessage `json:"input"`
	Model               string          `json:"model,omitempty"`
	SupportsInteraction bool            `json:"supports_interaction,omitempty"`
	// OutputFormat is the checked output_format member: the text.format every request of
	// the run sends. Nil when the request sent none.
	OutputFormat json.RawMessage `json:"output_format,omitempty"`
	// Plan starts the run in plan mode: read-only tools and the plan_mode instructions.
	Plan bool `json:"plan,omitempty"`
	// Tools, when set, narrows the run's tools to the ones these tools.allow entries
	// cover. Nil leaves the profile's tools as they are.
	Tools []string `json:"tools,omitempty"`
	// ExecutePlan sends plan_mode.execute_prompt, followed by the input when there is one:
	// the run carries out the plan the transcript ends with.
	ExecutePlan bool `json:"execute_plan,omitempty"`
}

const (
	bodyFirstChunk = 4 << 10
	bodyMaxChunk   = 1 << 20
)

// readAllChunked reads r to the end into chunks that grow as bytes arrive, then joins them
// once. A reader that doubles one buffer overshoots the body by up to 2x and copies it on
// every doubling; sizing the buffer from Content-Length instead lets a client that declares
// a large body and sends none pin that much memory.
func readAllChunked(r io.Reader) ([]byte, error) {
	var chunks [][]byte
	total, size := 0, bodyFirstChunk
	for {
		chunk := make([]byte, size)
		n, err := io.ReadFull(r, chunk)
		if n > 0 {
			chunks = append(chunks, chunk[:n])
			total += n
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
		size = min(size*2, bodyMaxChunk)
	}
	if len(chunks) == 1 {
		return chunks[0], nil
	}
	joined := make([]byte, 0, total)
	for _, chunk := range chunks {
		joined = append(joined, chunk...)
	}
	return joined, nil
}

func readRunBody(w http.ResponseWriter, r *http.Request, maxBytes, baseBytes int64, limits RunInputLimits) (runRequest, error) {
	raw, err := readAllChunked(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return runRequest{}, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "invalid JSON body"}
	}
	if !json.Valid(raw) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		var first json.RawMessage
		if decoder.Decode(&first) != nil {
			return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "invalid JSON body"}
		}
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "request body must contain one JSON object"}
	}
	if firstJSONByte(raw) != '{' {
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "input must be a non-empty string"}
	}
	// The members are located in raw itself; json.Valid has accepted it, so nothing is copied.
	members := scanRunBodyObject(raw)
	var body runRequest
	if members.hasExecutePlan {
		if json.Unmarshal(members.executePlan, &body.ExecutePlan) != nil || string(bytes.TrimSpace(members.executePlan)) == "null" {
			return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "execute_plan must be a boolean"}
		}
	}
	var input json.RawMessage
	var imageChars int64
	if body.ExecutePlan {
		// The input is optional and is text only: it follows the configured prompt.
		var text string
		if members.hasInput && json.Unmarshal(members.input, &text) != nil {
			return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "with execute_plan, input must be a string or left out"}
		}
		if strings.TrimSpace(text) == "" {
			// No text of its own: the runner sends the configured prompt alone.
			input, members.hasInput = json.RawMessage(`""`), true
		}
	}
	if !members.hasInput {
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "input must be a non-empty string"}
	}
	if input == nil {
		if input, imageChars, err = parseValidRunInput(members.input, limits); err != nil {
			return runRequest{}, err
		}
	}
	// The reader allowed the image data on top of max_body_bytes; everything else in the
	// body still has to fit in max_body_bytes itself.
	if int64(len(raw))-imageChars > baseBytes {
		return runRequest{}, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
	}
	body.Input = input
	if members.hasModel {
		if json.Unmarshal(members.model, &body.Model) != nil || strings.TrimSpace(body.Model) == "" {
			return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "model must be a non-empty string"}
		}
	}
	if members.hasSupports {
		_ = json.Unmarshal(members.supports, &body.SupportsInteraction)
	}
	if members.hasOutputFormat {
		if body.OutputFormat, err = parseRunOutputFormat(members.outputFormat); err != nil {
			return runRequest{}, err
		}
	}
	if members.hasPlan {
		if json.Unmarshal(members.plan, &body.Plan) != nil || string(bytes.TrimSpace(members.plan)) == "null" {
			return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "plan must be a boolean"}
		}
	}
	if body.Plan && body.ExecutePlan {
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "execute_plan carries out a plan, so it cannot run in plan mode"}
	}
	if members.hasTools {
		if body.Tools, err = parseRunTools(members.tools); err != nil {
			return runRequest{}, err
		}
	}
	body.Model = strings.TrimSpace(body.Model)
	return body, nil
}

// parseRunTools reads the run body's tools: an array of distinct tools.allow entries
// (a native tool name, mcp:<server>:<tool> or mcp:<server>:*). An empty array runs with
// no tools.
func parseRunTools(raw []byte) ([]string, error) {
	var entries []string
	if json.Unmarshal(raw, &entries) != nil || entries == nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "tools must be an array of tool names"}
	}
	for index, entry := range entries {
		if err := sessionfs.CheckToolEntry(entry); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("tools[%d] %v", index, err)}
		}
		if slices.Contains(entries[:index], entry) {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("tools[%d] repeats %q", index, entry)}
		}
	}
	return entries, nil
}

func decodedPathSegments(u *url.URL) ([]string, error) {
	path := u.EscapedPath()
	if path == "" || path == "/" {
		return nil, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	segments := make([]string, len(parts))
	for index, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return nil, err
		}
		segments[index] = decoded
	}
	return segments, nil
}

func profileParameter(u *url.URL) (string, error) {
	values, exists := u.Query()["profile"]
	if !exists || len(values) == 0 || values[0] == "" {
		return "", nil
	}
	if !profileNamePattern.MatchString(values[0]) {
		return "", &StatusError{Code: http.StatusBadRequest, Message: "profile must contain only letters, numbers, underscores, or hyphens"}
	}
	return values[0], nil
}

func normalizeResolverError(err error) error {
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		if statusErr.Code < http.StatusBadRequest || statusErr.Code > 599 || statusErr.Message == "" {
			return errors.New("session resolver returned an invalid HTTP error")
		}
		return statusErr
	}
	return err
}

func writeJSONError(w http.ResponseWriter, err error) {
	if recorder, ok := w.(causeRecorder); ok {
		recorder.recordCause(err)
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		if statusErr.RetryAfterSeconds > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(statusErr.RetryAfterSeconds))
		}
		writeJSON(w, statusErr.Code, map[string]string{"error": statusErr.Message})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// invalidSessionReferenceMessage returns the client-facing reason a request
// reference is malformed, using the same wording and check order as the
// TypeScript parseSessionReference, or "" when the reference is well formed.
func invalidSessionReferenceMessage(reference string) string {
	if sessionReferencePattern.MatchString(reference) {
		return ""
	}
	parts := strings.Split(reference, ":")
	switch len(parts) {
	case 1:
		return "invalid session id"
	case 2:
		if !sessionRootIDPattern.MatchString(parts[0]) {
			return "invalid session root id"
		}
		return "invalid session id"
	default:
		return "invalid session reference"
	}
}
