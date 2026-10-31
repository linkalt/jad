package remoteapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// Prefix is where every route lives.
	Prefix = "/api/remote/v1"
	// HeaderHops is the hop counter the client sends.
	HeaderHops = "X-Jad-Hops"

	DefaultMaxHops        = 3
	MaxMaxHops            = 16
	DefaultMaxRuns        = 256
	DefaultMaxRunsPerTok  = 16
	MaxMaxRuns            = 10_000
	DefaultRunRetentionMs = 10 * 60 * 1000
	MaxRunRetentionMs     = 24 * 60 * 60 * 1000
	DefaultWaitMaxMs      = 25_000
	MaxWaitMaxMs          = 60_000
	DefaultBodyMaxBytes   = 1 << 20
	MaxBodyMaxBytes       = 16 << 20
	DefaultResultMaxBytes = 256 << 10
	MaxResultMaxBytes     = 16 << 20
)

// Errors a Backend returns; anything else is an internal error and is not shown to the client.
var (
	ErrNoSession     = errors.New("session not found")
	ErrUnknownTarget = errors.New("unknown profile or agent")
	ErrBusy          = errors.New("target is busy")
	ErrRefused       = errors.New("request refused")
)

// Refusal is ErrRefused with a client-safe reason (cycle, chain too long, unavailable).
type Refusal struct{ Code, Message string }

func (r *Refusal) Error() string        { return r.Message }
func (r *Refusal) Is(target error) bool { return target == ErrRefused }

// MessageRequest is one message for a profile.
type MessageRequest struct {
	Session, Profile, Message string
	// Caller names who sent it ("remote:<token id>"); Hops is the hop count of the request.
	Caller string
	Hops   int
}

// SubagentRequest is one task for an agent of a profile's subagent tool; the profile gives
// the context (tools, agent list, working directory) the agent runs in.
type SubagentRequest struct {
	Session, Profile, Agent, Task string
	Caller                        string
	Hops                          int
}

// RunResult is how a run ended.
type RunResult struct {
	// State is done, failed, stopped or timed_out.
	State   string
	Text    string
	Code    string
	Message string
	Reason  string
}

// Run is a started piece of work. Result is valid after Done closes. Stop does not wait.
type Run interface {
	Done() <-chan struct{}
	Result() RunResult
	Stop(reason string)
}

// Backend is what the server needs from the application.
type Backend interface {
	// Profiles lists "category/name" of the profiles of an existing session.
	Profiles(ctx context.Context, session string) ([]string, error)
	SendMessage(ctx context.Context, request MessageRequest) (Run, error)
	StartSubagent(ctx context.Context, request SubagentRequest) (Run, error)
}

// TranscriptRequest is a read of transcripts for a token: Tool is "transcript" or "transcripts",
// Arguments the tool's input (already checked for keys and types), MayProfile whether the
// token's globs reach "session/category/profile" and MayAgent "session/category/profile/agent".
type TranscriptRequest struct {
	Tool       string
	Arguments  map[string]any
	MayProfile func(target string) bool
	MayAgent   func(target string) bool
}

// TranscriptBackend is the optional part of a Backend that serves transcripts: it runs the
// real transcript tools with file paths hidden and returns their rendered result.
type TranscriptBackend interface {
	Transcript(ctx context.Context, request TranscriptRequest) (string, error)
}

// Options configure a Server.
type Options struct {
	Tokens  *TokenTable
	Backend Backend
	Logger  *slog.Logger
	Now     func() time.Time
	// MaxHops is the server's hop limit; a token's own max_hops can only lower it.
	MaxHops int
	// MaxRuns bounds the runs held (live and finished); MaxRunsPerToken bounds the live runs of one token.
	MaxRuns         int
	MaxRunsPerToken int
	RunRetention    time.Duration
	WaitMax         time.Duration
	BodyMaxBytes    int64
	ResultMaxBytes  int
}

// Server serves /api/remote/v1.
type Server struct {
	opts Options
	mu   sync.Mutex
	runs map[string]*runEntry
	// requests maps token id + request id to the run it started, so a repeat is the same run.
	requests map[string]string
	closed   bool
}

type runEntry struct {
	id, token, requestKey, kind string
	// fingerprint is the hash of what the request asked for, so a reused request id with other content is refused.
	fingerprint [sha256.Size]byte
	run         Run
	started     time.Time
	ended       time.Time
	// ready closes when the start has finished; startErr is set when it failed.
	ready    chan struct{}
	startErr error
}

// NewServer applies defaults to the options.
func NewServer(opts Options) (*Server, error) {
	if opts.Tokens == nil || opts.Backend == nil {
		return nil, errors.New("remote api needs a token table and a backend")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxHops <= 0 {
		opts.MaxHops = DefaultMaxHops
	}
	if opts.MaxRuns <= 0 {
		opts.MaxRuns = DefaultMaxRuns
	}
	if opts.MaxRunsPerToken <= 0 {
		opts.MaxRunsPerToken = DefaultMaxRunsPerTok
	}
	if opts.RunRetention <= 0 {
		opts.RunRetention = DefaultRunRetentionMs * time.Millisecond
	}
	if opts.WaitMax <= 0 {
		opts.WaitMax = DefaultWaitMaxMs * time.Millisecond
	}
	if opts.BodyMaxBytes <= 0 {
		opts.BodyMaxBytes = DefaultBodyMaxBytes
	}
	if opts.ResultMaxBytes <= 0 {
		opts.ResultMaxBytes = DefaultResultMaxBytes
	}
	return &Server{opts: opts, runs: map[string]*runEntry{}, requests: map[string]string{}}, nil
}

// Close stops every live run and refuses new ones.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	live := make([]*runEntry, 0, len(s.runs))
	for _, entry := range s.runs {
		live = append(live, entry)
	}
	s.mu.Unlock()
	for _, entry := range live {
		s.mu.Lock()
		run := entry.run
		s.mu.Unlock()
		if run != nil {
			run.Stop("server_shutdown")
		}
	}
}

var (
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	runIDPattern     = regexp.MustCompile(`^rr_[0-9a-f]{16}$`)
	namePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	sessionPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), Prefix)
	if !ok || (rest != "" && rest[0] != '/') {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	token, err := s.opts.Tokens.Authenticate(r.Header.Get("Authorization"))
	if errors.Is(err, errTableBroken) {
		// The server cannot read its own token table, so the caller is not refused for its
		// token and gets no challenge; a followed run retries a 503 instead of ending.
		writeError(w, http.StatusServiceUnavailable, "token_table_unusable", "the server cannot check tokens right now")
		return
	}
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="jad-remote"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid token is required")
		return
	}
	hops, ok := s.hops(w, r, token)
	if !ok {
		return
	}
	segments := strings.Split(strings.Trim(rest, "/"), "/")
	switch {
	case r.Method == http.MethodGet && len(segments) == 1 && segments[0] == "profiles":
		s.handleProfiles(w, r, token)
	case r.Method == http.MethodPost && len(segments) == 1 && segments[0] == "messages":
		s.handleStart(w, r, token, hops, "message")
	case r.Method == http.MethodPost && len(segments) == 1 && segments[0] == "subagents":
		s.handleStart(w, r, token, hops, "subagent")
	case r.Method == http.MethodGet && len(segments) == 1 && (segments[0] == "transcript" || segments[0] == "transcripts"):
		s.handleTranscript(w, r, token, segments[0])
	case r.Method == http.MethodGet && len(segments) == 2 && segments[0] == "runs":
		s.handleRun(w, r, token, segments[1])
	case r.Method == http.MethodPost && len(segments) == 3 && segments[0] == "runs" && segments[2] == "stop":
		s.handleStop(w, r, token, segments[1])
	default:
		writeError(w, http.StatusNotFound, "not_found", "not found")
	}
}

// hops reads the hop counter and refuses a request past the limit.
func (s *Server) hops(w http.ResponseWriter, r *http.Request, token Token) (int, bool) {
	hops := 0
	if value := strings.TrimSpace(r.Header.Get(HeaderHops)); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > 1_000_000 {
			writeError(w, http.StatusBadRequest, "bad_request", HeaderHops+" must be a non-negative integer")
			return 0, false
		}
		hops = parsed
	}
	limit := s.opts.MaxHops
	if token.MaxHops > 0 && token.MaxHops < limit {
		limit = token.MaxHops
	}
	if hops > limit {
		s.opts.Logger.Warn("remote api: hop limit", "token", token.ID, "hops", hops, "limit", limit)
		writeError(w, http.StatusLoopDetected, "remote_hop_limit", "the request passed through too many JAD servers ("+strconv.Itoa(limit)+" allowed)")
		return 0, false
	}
	return hops, true
}

func (s *Server) deny(w http.ResponseWriter, token Token, action, what string) {
	s.opts.Logger.Warn("remote api: denied", "token", token.ID, "action", action, "target", what)
	writeError(w, http.StatusForbidden, "forbidden", "this token may not do that")
}

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request, token Token) {
	if !token.Allows(ActionMessage) && !token.Allows(ActionSubagent) && !token.Allows(ActionTranscripts) {
		s.deny(w, token, "profiles", "")
		return
	}
	session := r.URL.Query().Get("session")
	if !sessionPattern.MatchString(session) {
		writeError(w, http.StatusBadRequest, "bad_request", "session is required")
		return
	}
	names, err := s.opts.Backend.Profiles(r.Context(), session)
	if err != nil {
		s.backendError(w, token, err)
		return
	}
	visible := make([]string, 0, len(names))
	for _, name := range names {
		if token.MayProfile(session + "/" + name) {
			visible = append(visible, name)
		}
	}
	// A session the token cannot see answers like a missing one, so the status does not show
	// whether the session exists.
	if len(visible) == 0 {
		writeError(w, http.StatusNotFound, "no_session", "that session does not exist on this server")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session, "profiles": visible})
}

type startBody struct {
	Session   string `json:"session"`
	Profile   string `json:"profile"`
	Agent     string `json:"agent"`
	Message   string `json:"message"`
	Task      string `json:"task"`
	RequestID string `json:"request_id"`
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request, token Token, hops int, kind string) {
	action := ActionMessage
	if kind == "subagent" {
		action = ActionSubagent
	}
	if !token.Allows(action) {
		s.deny(w, token, action, "")
		return
	}
	var body startBody
	reader := http.MaxBytesReader(w, r.Body, s.opts.BodyMaxBytes)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object with the documented keys")
		return
	}
	if _, err := decoder.Token(); err != io.EOF {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be one JSON object")
		return
	}
	target, text := body.Profile, body.Message
	if kind == "subagent" {
		text = body.Task
	}
	valid := sessionPattern.MatchString(body.Session) && requestIDPattern.MatchString(body.RequestID) && strings.TrimSpace(text) != "" && validTarget(target)
	if kind == "subagent" {
		valid = valid && namePattern.MatchString(body.Agent)
	} else {
		valid = valid && body.Agent == "" && body.Task == ""
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "bad_request", "session, request_id (8 to 64 letters, digits, '_' or '-'), a category/name profile, an agent name (subagents only) and the text are required")
		return
	}
	scope := body.Session + "/" + target
	allowed := token.MayProfile(scope)
	if kind == "subagent" {
		scope += "/" + body.Agent
		allowed = token.MayAgent(scope)
	}
	if !allowed {
		s.deny(w, token, action, scope)
		return
	}
	key := token.ID + "\x00" + body.RequestID
	fingerprint := sha256.Sum256([]byte(strings.Join([]string{kind, body.Session, target, body.Agent, text}, "\x00")))
	entry, status := s.begin(r.Context(), token, key, kind, fingerprint, func(ctx context.Context) (Run, error) {
		caller := "remote:" + token.ID
		if kind == "subagent" {
			return s.opts.Backend.StartSubagent(ctx, SubagentRequest{Session: body.Session, Profile: target, Agent: body.Agent, Task: text, Caller: caller, Hops: hops})
		}
		return s.opts.Backend.SendMessage(ctx, MessageRequest{Session: body.Session, Profile: target, Message: text, Caller: caller, Hops: hops})
	})
	if entry == nil {
		s.startFailure(w, token, status.err)
		return
	}
	writeJSON(w, status.http, s.view(entry, false))
}

type beginStatus struct {
	http int
	err  error
}

// begin starts the run once per (token, request id): a repeat gets the same run back, so a
// retried send never starts the work twice. The start runs outside the lock; a repeat that
// arrives meanwhile waits for it.
func (s *Server) begin(ctx context.Context, token Token, key, kind string, fingerprint [sha256.Size]byte, start func(context.Context) (Run, error)) (*runEntry, beginStatus) {
	s.mu.Lock()
	s.sweepLocked()
	if s.closed {
		s.mu.Unlock()
		return nil, beginStatus{err: &Refusal{Code: "unavailable", Message: "the server is shutting down"}}
	}
	if id, ok := s.requests[key]; ok {
		if entry := s.runs[id]; entry != nil {
			s.mu.Unlock()
			if entry.fingerprint != fingerprint {
				return nil, beginStatus{err: &Refusal{Code: "request_id_reused", Message: "this request_id was already used for a different request"}}
			}
			select {
			case <-entry.ready:
			case <-ctx.Done():
				return nil, beginStatus{err: ctx.Err()}
			}
			if entry.startErr != nil {
				return nil, beginStatus{err: entry.startErr}
			}
			return entry, beginStatus{http: http.StatusOK}
		}
	}
	live := 0
	for _, held := range s.runs {
		if held.token == token.ID && held.ended.IsZero() {
			live++
		}
	}
	if live >= s.opts.MaxRunsPerToken || (len(s.runs) >= s.opts.MaxRuns && !s.evictFinishedLocked(token.ID)) {
		s.mu.Unlock()
		return nil, beginStatus{err: &Refusal{Code: "too_many_runs", Message: "too many remote runs are going; wait for some to end"}}
	}
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	entry := &runEntry{id: "rr_" + hex.EncodeToString(random), token: token.ID, requestKey: key, kind: kind, fingerprint: fingerprint, started: s.opts.Now(), ready: make(chan struct{})}
	s.runs[entry.id] = entry
	s.requests[key] = entry.id
	s.mu.Unlock()

	run, err := start(ctx)
	s.mu.Lock()
	if err != nil {
		entry.startErr = err
		delete(s.runs, entry.id)
		delete(s.requests, key)
		s.mu.Unlock()
		close(entry.ready)
		return nil, beginStatus{err: err}
	}
	entry.run = run
	closed := s.closed
	s.mu.Unlock()
	close(entry.ready)
	if closed {
		run.Stop("server_shutdown")
	}
	go func() {
		<-run.Done()
		s.mu.Lock()
		entry.ended = s.opts.Now()
		s.mu.Unlock()
	}()
	return entry, beginStatus{http: http.StatusAccepted}
}

// evictFinishedLocked drops the oldest finished run to make room, the token's own first so one
// token's churn does not erase another's results; false when every held run is still live.
func (s *Server) evictFinishedLocked(tokenID string) bool {
	var oldest *runEntry
	for _, held := range s.runs {
		if held.ended.IsZero() {
			continue
		}
		if oldest == nil || (held.token == tokenID) != (oldest.token == tokenID) && held.token == tokenID ||
			(held.token == tokenID) == (oldest.token == tokenID) && held.ended.Before(oldest.ended) {
			oldest = held
		}
	}
	if oldest == nil {
		return false
	}
	delete(s.runs, oldest.id)
	delete(s.requests, oldest.requestKey)
	return true
}

// sweepLocked forgets finished runs past their retention.
func (s *Server) sweepLocked() {
	now := s.opts.Now()
	for id, entry := range s.runs {
		if !entry.ended.IsZero() && now.Sub(entry.ended) > s.opts.RunRetention {
			delete(s.runs, id)
			delete(s.requests, entry.requestKey)
		}
	}
}

func validTarget(target string) bool {
	category, name, ok := strings.Cut(target, "/")
	return ok && namePattern.MatchString(category) && namePattern.MatchString(name)
}

func (s *Server) startFailure(w http.ResponseWriter, token Token, err error) {
	var refusal *Refusal
	switch {
	case errors.As(err, &refusal):
		status := http.StatusConflict
		if refusal.Code == "too_many_runs" {
			status = http.StatusTooManyRequests
		} else if refusal.Code == "unavailable" {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, refusal.Code, refusal.Message)
	case errors.Is(err, ErrBusy):
		writeError(w, http.StatusConflict, "profile_busy", "the target is busy; try again later")
	default:
		s.backendError(w, token, err)
	}
}

func (s *Server) backendError(w http.ResponseWriter, token Token, err error) {
	switch {
	case errors.Is(err, ErrNoSession):
		writeError(w, http.StatusNotFound, "no_session", "that session does not exist on this server")
	case errors.Is(err, ErrUnknownTarget):
		writeError(w, http.StatusNotFound, "unknown_target", "that profile or agent does not exist in the session")
	default:
		s.opts.Logger.Error("remote api: backend error", "token", token.ID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "the server could not do that")
	}
}

func (s *Server) lookup(token Token, id string) *runEntry {
	if !runIDPattern.MatchString(id) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	entry := s.runs[id]
	if entry == nil || entry.token != token.ID || entry.run == nil {
		return nil
	}
	return entry
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request, token Token, id string) {
	if !token.Allows(ActionStatus) {
		s.deny(w, token, ActionStatus, id)
		return
	}
	entry := s.lookup(token, id)
	if entry == nil {
		writeError(w, http.StatusNotFound, "no_run", "no such run")
		return
	}
	if value := r.URL.Query().Get("wait_ms"); value != "" {
		wait, err := strconv.Atoi(value)
		if err != nil || wait < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "wait_ms must be a non-negative integer")
			return
		}
		limit := min(time.Duration(wait)*time.Millisecond, s.opts.WaitMax)
		timer := time.NewTimer(limit)
		select {
		case <-entry.run.Done():
		case <-timer.C:
		case <-r.Context().Done():
		}
		timer.Stop()
	}
	writeJSON(w, http.StatusOK, s.view(entry, true))
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request, token Token, id string) {
	if !token.Allows(ActionStop) {
		s.deny(w, token, ActionStop, id)
		return
	}
	entry := s.lookup(token, id)
	if entry == nil {
		writeError(w, http.StatusNotFound, "no_run", "no such run")
		return
	}
	entry.run.Stop("remote_stop")
	writeJSON(w, http.StatusOK, s.view(entry, false))
}

// runView is the JSON of a run. Text is the answer (cut to result_max_bytes) once it ended.
type runView struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Kind      string `json:"kind"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Text      string `json:"text,omitempty"`
	Cut       bool   `json:"cut,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Server) view(entry *runEntry, withText bool) runView {
	view := runView{ID: entry.id, Kind: entry.kind, State: "running"}
	select {
	case <-entry.run.Done():
		result := entry.run.Result()
		view.State, view.Code, view.Message, view.Reason = result.State, result.Code, safe(result.Message, 1024), safe(result.Reason, 128)
		view.Text, view.Cut = cutUTF8(result.Text, s.opts.ResultMaxBytes)
		s.mu.Lock()
		ended := entry.ended
		s.mu.Unlock()
		if ended.IsZero() {
			ended = s.opts.Now()
		}
		view.ElapsedMs = ended.Sub(entry.started).Milliseconds()
	default:
		view.ElapsedMs = s.opts.Now().Sub(entry.started).Milliseconds()
	}
	return view
}

func cutUTF8(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

func safe(text string, limit int) string {
	text, _ = cutUTF8(text, limit)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, text)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

// transcriptKeys are the query keys of each route; "int" keys are numbers, "list" repeats.
var transcriptKeys = map[string]map[string]string{
	"transcript":  {"path": "", "turns": "", "source": "", "role": "", "tool": "", "search": "", "context": "int", "step": "", "part": "", "cursor": ""},
	"transcripts": {"path": "", "filters": "list", "status": "", "limit": "int", "cursor": ""},
}

const transcriptValueMax = 2048

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request, token Token, tool string) {
	backend, ok := s.opts.Backend.(TranscriptBackend)
	if !ok || !token.Allows(ActionTranscripts) {
		s.deny(w, token, ActionTranscripts, tool)
		return
	}
	keys := transcriptKeys[tool]
	arguments := map[string]any{}
	for key, values := range r.URL.Query() {
		kind, known := keys[key]
		if !known {
			writeError(w, http.StatusBadRequest, "bad_request", key+" is not a "+tool+" key")
			return
		}
		for _, value := range values {
			if len(value) > transcriptValueMax || strings.ContainsRune(value, 0) {
				writeError(w, http.StatusBadRequest, "bad_request", key+" is too long or not text")
				return
			}
		}
		switch kind {
		case "list":
			list := make([]any, 0, len(values))
			for _, value := range values {
				list = append(list, value)
			}
			arguments[key] = list
		case "int":
			number, err := strconv.ParseInt(values[0], 10, 32)
			if len(values) != 1 || err != nil || number < 0 {
				writeError(w, http.StatusBadRequest, "bad_request", key+" must be a non-negative integer")
				return
			}
			arguments[key] = json.Number(values[0])
		default:
			if len(values) != 1 || values[0] == "" {
				writeError(w, http.StatusBadRequest, "bad_request", key+" must be given once and not empty")
				return
			}
			arguments[key] = values[0]
		}
	}
	output, err := backend.Transcript(r.Context(), TranscriptRequest{Tool: tool, Arguments: arguments, MayProfile: token.MayProfile, MayAgent: token.MayAgent})
	if err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			writeError(w, http.StatusBadRequest, refusal.Code, refusal.Message)
			return
		}
		s.backendError(w, token, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": output})
}
