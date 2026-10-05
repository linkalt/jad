package httpapi

import (
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
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/agent-harness/backend-go/internal/contentlibrary"
	"example.com/agent-harness/backend-go/internal/logging"
	"example.com/agent-harness/backend-go/internal/memorytrace"
	"example.com/agent-harness/backend-go/internal/serverconfig"
	"example.com/agent-harness/backend-go/internal/session"
	"example.com/agent-harness/backend-go/internal/tools"
)

const (
	minAPITokenLength        = 32
	maxSafeJSONInteger int64 = 9007199254740991
)

var (
	sessionRootIDPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sessionReferencePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$|^[A-Za-z0-9][A-Za-z0-9._-]{0,63}:[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	profileNamePattern           = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
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
	SSEHeartbeatInterval       time.Duration
	RunStateRetention          time.Duration
	AllowZeroRunStateRetention bool
	ToolStreamInterval         time.Duration
	ConditionalRequests        bool
	SnapshotTurns              int
	MaxPageTurns               int
	MaxInlinePartBytes         int
	RunContentMemoryBytes      int
	RunContentWriteBatchBytes  int
	RunContentFlushInterval    time.Duration
	// MaxIndexSegments is accepted for root-config parity; the current Go
	// output_text cache merges into one part and does not need a range index.
	RunContentMaxIndexSegments int
	MemoryTrace                *memorytrace.Trace
}

// RunSession is the small session boundary used by the HTTP adapter.
type RunSession interface {
	Run(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error)
}

// ResolvedSession identifies the canonical session/profile key used for run
// attachment and provides the already configured session runner.
type ResolvedSession struct {
	Reference string
	Profile   string
	Runner    RunSession
}

// SessionResolver maps a requested session/profile/model path to a session
// runner. ModelPath is empty when resolving an attach or stop request.
type SessionResolver interface {
	Resolve(context.Context, string, string, string) (ResolvedSession, error)
}

// SessionProcessLocker is an optional adapter for serializing mutations across
// backend processes. Its lock is held across a full run or maintenance action.
type SessionProcessLocker interface {
	LockSession(context.Context, string, string, string) (func(), error)
	LockSessionSettings(context.Context, string) (func(), error)
}

// SnapshotOptions selects one page of transcript-backed session messages.
type SnapshotOptions struct {
	Before           *int64
	CursorGeneration string
	Turns            *int
}

// CatalogResponse is a JSON body and its validator for a read-only catalog route.
type CatalogResponse struct {
	Body        json.RawMessage
	ETag        string
	NotModified bool
}

// SessionRunState describes one retained run for the frontend.
type SessionRunState struct {
	TurnID    string `json:"turn_id"`
	StartedAt string `json:"started_at"`
	Status    string `json:"status"`
}

// SessionRunSummaries carries the stored-default run and all per-profile runs.
type SessionRunSummaries struct {
	Default   *SessionRunState
	ByProfile map[string]SessionRunState
}

// SessionRunProvider exposes run state without coupling the app resolver to the HTTP registry.
type SessionRunProvider interface {
	SessionRuns(reference, defaultProfile string) SessionRunSummaries
}

// SessionCatalog is an optional read-only extension implemented alongside the
// run resolver. Keeping it separate lets the HTTP handler remain testable with
// small run-only resolvers.
type SessionCatalog interface {
	ListSessions(context.Context, string, SessionRunProvider) (CatalogResponse, error)
	ReadSessionSnapshot(context.Context, string, string, SnapshotOptions, string, SessionRunProvider) (CatalogResponse, error)
	ReadSessionHistory(context.Context, string, string, string) (CatalogResponse, error)
	ReadSessionMessage(context.Context, string, string, string, *int64) (json.RawMessage, error)
}

// SessionToolLister returns the effective provider-neutral tool definitions for
// one resolved session/profile. It is separate from SessionResolver so small
// run-only adapters can remain independently testable.
type SessionToolLister interface {
	ListTools(context.Context, string, string) ([]tools.Definition, error)
}

type SessionPathProfileInfo struct {
	ActiveProfile string   `json:"active_profile,omitempty"`
	PathProfiles  []string `json:"path_profiles"`
}

type SessionPathProfileUpdate struct {
	ActiveProfile string    `json:"active_profile"`
	PathProfiles  []string  `json:"path_profiles"`
	ModelDefault  *string   `json:"model_default,omitempty"`
	Models        *[]string `json:"models,omitempty"`
	Changed       bool      `json:"changed"`
}

type SessionModelInfo struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type SessionModelDefaultUpdate struct {
	ActiveProfile string           `json:"active_profile"`
	PathProfiles  []string         `json:"path_profiles"`
	ModelDefault  string           `json:"model_default"`
	Models        []string         `json:"models"`
	Model         SessionModelInfo `json:"model"`
	Changed       bool             `json:"changed"`
}

type SessionFileListing struct {
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

// SessionSettings is an optional filesystem-backed extension for session
// profile/model changes and safe session-file suggestions.
type SessionSettings interface {
	ReadPathProfileInfo(context.Context, string) (SessionPathProfileInfo, error)
	UpdatePathProfile(context.Context, string, string) (SessionPathProfileUpdate, error)
	UpdateModelDefault(context.Context, string, string, string) (SessionModelDefaultUpdate, error)
	ListSessionFiles(context.Context, string, string) (SessionFileListing, error)
}

type CompactionResponse struct {
	Compacted      bool            `json:"compacted"`
	CompactedItems int             `json:"compacted_items"`
	RetainedItems  int             `json:"retained_items"`
	Usage          json.RawMessage `json:"usage,omitempty"`
}

type FrontCompactionResponse struct {
	DeleteMode  string `json:"delete_mode"`
	ArchiveName string `json:"archive_name,omitempty"`
}

type SessionMutation interface {
	Compact(context.Context) (CompactionResponse, error)
	Reset(context.Context) error
	FrontCompact(context.Context, string) (FrontCompactionResponse, error)
}

type ResolvedMutation struct {
	Reference string
	Profile   string
	Session   SessionMutation
}

type SessionMaintenance interface {
	ResolveMaintenance(context.Context, string, string) (ResolvedMutation, error)
}

// RootLibrary provides the optional, read-only global Markdown library.
type RootLibrary interface {
	List(context.Context) (contentlibrary.Catalog, error)
	Read(context.Context, contentlibrary.Kind, string) (contentlibrary.Asset, bool, error)
}

// StatusError lets a resolver return a safe, intentional HTTP error without
// coupling its implementation to this package's routing logic.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// Handler implements the health and session-run HTTP/SSE routes.
type Handler struct {
	logger                    *slog.Logger
	resolver                  SessionResolver
	catalog                   SessionCatalog
	toolLister                SessionToolLister
	settings                  SessionSettings
	maintenance               SessionMaintenance
	library                   RootLibrary
	tokenHash                 [sha256.Size]byte
	allowedOrigin             map[string]struct{}
	maxBodyBytes              int64
	toolStreamInterval        time.Duration
	sseHeartbeatInterval      time.Duration
	conditionalRequests       bool
	snapshotTurns             int
	maxPageTurns              int
	maxInlinePartBytes        int
	memoryTrace               *memorytrace.Trace
	runContentMemoryBytes     int
	runContentWriteBatchBytes int
	runContentFlushInterval   time.Duration
	runs                      *runRegistry
	sessionLocks              keyedLocker
	mutationMu                sync.Mutex
	mutations                 map[string]struct{}
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
	if config.MaxPageTurns < 1 {
		return nil, errors.New("maximum page turns must be positive")
	}
	if config.MaxInlinePartBytes < 0 {
		return nil, errors.New("maximum inline part bytes must not be negative")
	}
	if config.RunContentMemoryBytes < 0 || config.RunContentMemoryBytes > serverconfig.MaxRunContentMemoryBytes {
		return nil, errors.New("run content memory bytes must be between 0 and 16777216")
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
	settings, _ := resolver.(SessionSettings)
	maintenance, _ := resolver.(SessionMaintenance)
	trace := config.MemoryTrace
	if trace == nil {
		trace = memorytrace.FromEnvironment()
	}
	return &Handler{
		logger:                    logging.Or(config.Logger),
		resolver:                  resolver,
		catalog:                   catalog,
		toolLister:                toolLister,
		settings:                  settings,
		maintenance:               maintenance,
		library:                   library,
		tokenHash:                 sha256.Sum256([]byte(config.APIToken)),
		allowedOrigin:             origins,
		maxBodyBytes:              config.MaxBodyBytes,
		toolStreamInterval:        config.ToolStreamInterval,
		sseHeartbeatInterval:      config.SSEHeartbeatInterval,
		conditionalRequests:       config.ConditionalRequests,
		snapshotTurns:             config.SnapshotTurns,
		maxPageTurns:              config.MaxPageTurns,
		maxInlinePartBytes:        config.MaxInlinePartBytes,
		memoryTrace:               trace,
		runContentMemoryBytes:     config.RunContentMemoryBytes,
		runContentWriteBatchBytes: config.RunContentWriteBatchBytes,
		runContentFlushInterval:   config.RunContentFlushInterval,
		runs:                      newRunRegistry(config.RunStateRetention, config.SSEMaxBufferedBytes),
		mutations:                 make(map[string]struct{}),
	}, nil
}

// Close cancels active session runs. Call it during server shutdown.
func (h *Handler) Close() {
	if h != nil && h.runs != nil {
		h.runs.close()
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
			w.Header().Set("WWW-Authenticate", `Bearer realm="agent-harness"`)
			writeJSONError(w, &StatusError{Code: http.StatusUnauthorized, Message: "API token required"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h.handleRootLibrary(w, r, segments)
		return
	}
	if len(segments) < 2 || segments[0] != "api" || segments[1] != "sessions" {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "not found"})
		return
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agent-harness"`)
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
	case r.Method == http.MethodGet && len(segments) == 4 && segments[3] == "files":
		h.handleSessionFiles(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "profile":
		h.handleSessionProfile(w, r, reference)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "model-default":
		h.handleSessionModelDefault(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "compact":
		h.handleSessionCompact(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "reset":
		h.handleSessionReset(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "front-compact":
		h.handleSessionFrontCompact(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 6 && segments[3] == "run" && segments[4] == "interaction" && segments[5] == "extend":
		h.handleRunInteractionExtend(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 5 && segments[3] == "run" && segments[4] == "interaction":
		h.handleRunInteraction(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 4 && segments[3] == "run":
		h.handleRunStart(w, r, reference, profile)
	case r.Method == http.MethodGet && len(segments) == 5 && segments[3] == "run" && segments[4] == "stream":
		h.handleRunStream(w, r, reference, profile)
	case r.Method == http.MethodPost && len(segments) == 5 && segments[3] == "run" && segments[4] == "stop":
		h.handleRunStop(w, r, reference, profile)
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
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-None-Match")
	w.Header().Set("Access-Control-Expose-Headers", "ETag")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
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
	body, err := readRunBody(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
		return
	}

	var resolved ResolvedSession
	var input []byte
	var run *activeRun
	var processUnlock func()
	var startErr error
	func() {
		unlock := h.sessionLocks.lock(reference)
		defer unlock()
		processUnlock, startErr = h.lockSessionProcess(r.Context(), reference, profile, body.Model)
		if startErr != nil {
			return
		}
		resolved, startErr = h.resolve(r.Context(), reference, profile, body.Model)
		if startErr != nil {
			return
		}
		input, startErr = json.Marshal(body.Input)
		if startErr != nil {
			startErr = &StatusError{Code: http.StatusBadRequest, Message: "input could not be encoded"}
			return
		}
		turnID, err := newWebTurnID()
		if err != nil {
			startErr = &StatusError{Code: http.StatusInternalServerError, Message: "could not create run ID"}
			return
		}
		run, err = h.beginRun(h.runKey(resolved), turnID)
		if err != nil {
			var statusErr *StatusError
			if errors.As(err, &statusErr) {
				startErr = statusErr
			} else {
				startErr = &StatusError{Code: http.StatusServiceUnavailable, Message: "server is shutting down"}
			}
		}
	}()
	if startErr != nil {
		if processUnlock != nil {
			processUnlock()
		}
		writeJSONError(w, startErr)
		return
	}
	run.mu.Lock()
	run.supportsInteraction = body.SupportsInteraction
	run.mu.Unlock()
	contentCache := newRunContentCache(h.runContentMemoryBytes, h.runContentWriteBatchBytes, h.runContentFlushInterval)
	contentCache.logger = h.logger.With(runLogAttrs(run)...)
	run.setContentCache(contentCache)
	queue := run.subscribe()
	go func() {
		defer processUnlock()
		h.executeRun(run, resolved.Runner, input)
	}()
	h.serveSSE(w, r, run, queue)
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
	writeJSON(w, http.StatusOK, map[string]any{"stop": stop})
}

func (h *Handler) handleSessionTools(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.toolLister == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session tool listing is unavailable"})
		return
	}
	definitions, err := h.toolLister.ListTools(r.Context(), reference, profile)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": definitions})
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
	h.writeSSEHeaders(w)
	flusher.Flush()
	for {
		event, ok, quiet := queue.nextOrIdle(r.Context(), h.sseHeartbeatInterval)
		if quiet {
			// A comment frame carries no event, so a client parser ignores it; it
			// only proves the connection is alive during a long silent tool call.
			if _, err := w.Write(sseKeepAliveFrame); err != nil {
				return
			}
			flusher.Flush()
			continue
		}
		if !ok {
			return
		}
		if _, err := w.Write(event.frame); err != nil {
			return
		}
		flusher.Flush()
		if event.name == "done" {
			return
		}
	}
}

// sseKeepAliveFrame is the SSE comment sent on a quiet run stream.
var sseKeepAliveFrame = []byte(": keep-alive\n\n")

func (h *Handler) writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) runKey(resolved ResolvedSession) string {
	return resolved.Reference + "\x00" + resolved.Profile
}

type runRequest struct {
	Input               string `json:"input"`
	Model               string `json:"model,omitempty"`
	SupportsInteraction bool   `json:"supports_interaction,omitempty"`
}

func readRunBody(w http.ResponseWriter, r *http.Request, maxBytes int64) (runRequest, error) {
	reader := http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(reader)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return runRequest{}, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "invalid JSON body"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return runRequest{}, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "request body must contain one JSON object"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "input must be a non-empty string"}
	}

	inputRaw, exists := fields["input"]
	var body runRequest
	if !exists || json.Unmarshal(inputRaw, &body.Input) != nil || strings.TrimSpace(body.Input) == "" {
		return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "input must be a non-empty string"}
	}
	if modelRaw, exists := fields["model"]; exists {
		if json.Unmarshal(modelRaw, &body.Model) != nil || strings.TrimSpace(body.Model) == "" {
			return runRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "model must be a non-empty string"}
		}
	}
	if supportsRaw, exists := fields["supports_interaction"]; exists {
		_ = json.Unmarshal(supportsRaw, &body.SupportsInteraction)
	}
	body.Model = strings.TrimSpace(body.Model)
	return body, nil
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
