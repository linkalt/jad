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
	ErrNoSession = errors.New("session not found")
	// ErrAmbiguousSession is a bare session id that more than one workspace of the server holds.
	// The caller writes <workspace>:<session>. It is shown only to a token that already reaches
	// that session id (see handleProfiles for the one route that cannot know).
	ErrAmbiguousSession = errors.New("session id is in more than one workspace")
	ErrUnknownTarget    = errors.New("unknown profile or agent")
	// ErrForbidden is a request the server's own policy refuses (a profile_message deny rule of
	// the workspace or of global.json): it answers exactly as a token whose globs do not reach the
	// target does, so the remote caller learns no more than that.
	ErrForbidden = errors.New("request not allowed")
	ErrBusy      = errors.New("target is busy")
	ErrRefused   = errors.New("request refused")
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
	// Resume is the run a new run continues from: the remote's own id for it ("rr_…"), or ""
	// for none. The backend checks it against the caller.
	Resume string
	// RunID is the id this server minted for the run ("rr_…"). The backend keeps it with the
	// run it starts, so a later resume that names it can find that run.
	RunID  string
	Caller string
	Hops   int
}

// RunResult is how a run ended.
type RunResult struct {
	// State is done, failed, stopped or timed_out.
	State string
	// Text is the run's whole answer (or the partial text of a run that did not finish); the
	// view cuts it, and the result route hands it out whole in parts.
	Text    string
	Code    string
	Message string
	Reason  string
	// ResumedFrom is the run this one continues from, and ConfigNote the config note of a
	// resume; both are empty when they do not apply.
	ResumedFrom string
	ConfigNote  string
	// Worktree are the worktree_* lines of a run that worked in a git worktree of its own
	// (sandbox.isolation.mode worktree): where its work is on this server's repository.
	Worktree []WorktreeLine
}

// WorktreeLine is one worktree_* line of a run's result.
type WorktreeLine struct {
	Key   string `json:"key"`
	Value string `json:"value"`
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

// ProfileRequest asks for a profile's info: Session and Profile name it ("category/name"),
// MayProfile tells whether the token's globs reach "session/category/profile".
type ProfileRequest struct {
	Session    string
	Profile    string
	MayProfile func(target string) bool
}

// ProfileBackend is the optional part of a Backend that serves a profile's info. A remote can
// ask for info only: compact and reset are not offered here.
type ProfileBackend interface {
	ProfileInfo(ctx context.Context, request ProfileRequest) (string, error)
}

// Options configure a Server.
type Options struct {
	Tokens  *TokenTable
	Backend Backend
	Logger  *slog.Logger
	Now     func() time.Time
	// Random fills the bytes of a run ID; nil means crypto/rand.
	Random func([]byte) (int, error)
	// MaxHops is the server's hop limit; a token's own max_hops can only lower it.
	MaxHops int
	// MaxRuns bounds the runs held (live and finished); MaxRunsPerToken bounds the live runs of one token.
	MaxRuns         int
	MaxRunsPerToken int
	RunRetention    time.Duration
	WaitMax         time.Duration
	BodyMaxBytes    int64
	ResultMaxBytes  int
	// Workspaces are the ids of the workspaces this server serves (the workspace layout); empty
	// for a v1 server. A token row of the workspace layout reaches one of them.
	Workspaces []string
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
	if opts.Random == nil {
		opts.Random = rand.Read
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
	// sessionPattern is a session id or <workspace>:<session>, the root id the server lists
	// for a workspace (a v1 server with several session roots takes <root>:<session> alike).
	sessionPattern = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]{0,63}:)?[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
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
	case r.Method == http.MethodGet && len(segments) == 1 && segments[0] == "profile":
		s.handleProfile(w, r, token)
	case r.Method == http.MethodGet && len(segments) == 2 && segments[0] == "runs":
		s.handleRun(w, r, token, segments[1])
	case r.Method == http.MethodGet && len(segments) == 3 && segments[0] == "runs" && segments[2] == "result":
		s.handleResult(w, r, token, segments[1])
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
	bare, reference, inside := s.qualify(token, session)
	if !inside {
		writeError(w, http.StatusNotFound, "no_session", "that session does not exist on this server")
		return
	}
	names, err := s.opts.Backend.Profiles(r.Context(), reference)
	if errors.Is(err, ErrAmbiguousSession) {
		// The token is checked against profile names only after the list is read, so naming the
		// ambiguity here would tell any token that a session id exists in two workspaces. The
		// list answers as a missing session; a start or info request, which the token's globs
		// have already admitted, names the ambiguity.
		err = ErrNoSession
	}
	if err != nil {
		s.backendError(w, token, err)
		return
	}
	visible := make([]string, 0, len(names))
	for _, name := range names {
		if token.MayProfile(bare + "/" + name) {
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
	Resume    string `json:"resume"`
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
	textMax, ok := s.textMax(w, r)
	if !ok {
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
		valid = valid && namePattern.MatchString(body.Agent) && (body.Resume == "" || runIDPattern.MatchString(body.Resume))
	} else {
		valid = valid && body.Agent == "" && body.Task == "" && body.Resume == ""
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "bad_request", "session, request_id (8 to 64 letters, digits, '_' or '-'), a category/name profile, an agent name (subagents only) and the text are required")
		return
	}
	bare, reference, inside := s.qualify(token, body.Session)
	if !inside {
		writeError(w, http.StatusNotFound, "no_session", "that session does not exist on this server")
		return
	}
	scope := bare + "/" + target
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
	fingerprint := sha256.Sum256([]byte(strings.Join([]string{kind, reference, target, body.Agent, text, body.Resume}, "\x00")))
	entry, status := s.begin(r.Context(), token, key, kind, fingerprint, func(ctx context.Context, id string) (Run, error) {
		caller := "remote:" + token.ID
		if kind == "subagent" {
			return s.opts.Backend.StartSubagent(ctx, SubagentRequest{Session: reference, Profile: target, Agent: body.Agent, Task: text, Resume: body.Resume, RunID: id, Caller: caller, Hops: hops})
		}
		return s.opts.Backend.SendMessage(ctx, MessageRequest{Session: reference, Profile: target, Message: text, Caller: caller, Hops: hops})
	})
	if entry == nil {
		s.startFailure(w, token, status.err)
		return
	}
	writeJSON(w, status.http, s.view(entry, textMax))
}

type beginStatus struct {
	http int
	err  error
}

// begin starts the run once per (token, request id): a repeat gets the same run back, so a
// retried send never starts the work twice. The start runs outside the lock; a repeat that
// arrives meanwhile waits for it.
func (s *Server) begin(ctx context.Context, token Token, key, kind string, fingerprint [sha256.Size]byte, start func(context.Context, string) (Run, error)) (*runEntry, beginStatus) {
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
	if _, err := s.opts.Random(random); err != nil {
		s.mu.Unlock()
		return nil, beginStatus{err: &Refusal{Code: "unavailable", Message: "the server could not create a run ID; try again later"}}
	}
	entry := &runEntry{id: "rr_" + hex.EncodeToString(random), token: token.ID, requestKey: key, kind: kind, fingerprint: fingerprint, started: s.opts.Now(), ready: make(chan struct{})}
	s.runs[entry.id] = entry
	s.requests[key] = entry.id
	s.mu.Unlock()

	run, err := start(ctx, entry.id)
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
	case errors.Is(err, ErrAmbiguousSession):
		writeError(w, http.StatusConflict, "ambiguous_session", "that session id is in more than one workspace of this server; write <workspace>:<session>")
	case errors.Is(err, ErrUnknownTarget):
		writeError(w, http.StatusNotFound, "unknown_target", "that profile or agent does not exist in the session")
	case errors.Is(err, ErrForbidden):
		s.opts.Logger.Warn("remote api: denied by policy", "token", token.ID)
		writeError(w, http.StatusForbidden, "forbidden", "this token may not do that")
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
	textMax, ok := s.textMax(w, r)
	if !ok {
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
	writeJSON(w, http.StatusOK, s.view(entry, textMax))
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
	textMax, ok := s.textMax(w, r)
	if !ok {
		return
	}
	entry.run.Stop("remote_stop")
	writeJSON(w, http.StatusOK, s.view(entry, textMax))
}

// textMax reads the text_max_bytes query of a run view: the most text the caller can take in
// the view (its answer limit divided by how far JSON may grow a text). It is the server's
// result_max_bytes when absent, and never more than that; a caller that cannot take the whole
// answer in a view reads the rest from the result route. It answers 400 for a bad value.
func (s *Server) textMax(w http.ResponseWriter, r *http.Request) (int, bool) {
	value := r.URL.Query().Get("text_max_bytes")
	if value == "" {
		return s.opts.ResultMaxBytes, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "text_max_bytes must be a non-negative integer")
		return 0, false
	}
	return min(limit, s.opts.ResultMaxBytes), true
}

// handleResult serves the whole text of an ended run, up to limit bytes from offset, as plain
// bytes (a chunk may end inside a character; the parts joined are the text). X-Result-Bytes is
// the size of the whole text, so a reader knows when it has all of it. This is how a caller
// gets an answer longer than the view's result_max_bytes, in parts it can take.
func (s *Server) handleResult(w http.ResponseWriter, r *http.Request, token Token, id string) {
	if !token.Allows(ActionStatus) {
		s.deny(w, token, ActionStatus, id)
		return
	}
	entry := s.lookup(token, id)
	if entry == nil {
		writeError(w, http.StatusNotFound, "no_run", "no such run")
		return
	}
	offset, limit := 0, s.opts.ResultMaxBytes
	query := r.URL.Query()
	for key, target := range map[string]*int{"offset": &offset, "limit": &limit} {
		value := query.Get(key)
		if value == "" {
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil || number < 0 || (key == "limit" && number == 0) {
			writeError(w, http.StatusBadRequest, "bad_request", key+" must be a positive integer (offset may be 0)")
			return
		}
		*target = number
	}
	limit = min(limit, MaxResultMaxBytes)
	select {
	case <-entry.run.Done():
	default:
		writeError(w, http.StatusConflict, "not_finished", "the run has not ended")
		return
	}
	text := entry.run.Result().Text
	if offset > len(text) {
		writeError(w, http.StatusBadRequest, "bad_request", "offset is past the end of the result")
		return
	}
	part := text[offset:min(len(text), offset+limit)]
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Result-Bytes", strconv.Itoa(len(text)))
	w.Header().Set("Content-Length", strconv.Itoa(len(part)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, part)
}

// runView is the JSON of a run. Text is the answer cut to result_max_bytes (or to the text_max_bytes
// the caller asked for) once it ended; cut says it was cut.
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
	// ResumedFrom and ConfigNote are set for a run that resumed another, and only then.
	ResumedFrom string `json:"resumed_from,omitempty"`
	ConfigNote  string `json:"config_note,omitempty"`
	// Worktree is set for a run that worked in a git worktree of its own, and only then.
	Worktree []WorktreeLine `json:"worktree,omitempty"`
}

func (s *Server) view(entry *runEntry, textMax int) runView {
	view := runView{ID: entry.id, Kind: entry.kind, State: "running"}
	select {
	case <-entry.run.Done():
		result := entry.run.Result()
		view.State, view.Code, view.Message, view.Reason = result.State, result.Code, safe(result.Message, 1024), safe(result.Reason, 128)
		view.ResumedFrom, view.ConfigNote = result.ResumedFrom, result.ConfigNote
		for _, line := range result.Worktree {
			view.Worktree = append(view.Worktree, WorktreeLine{Key: safe(line.Key, 64), Value: safe(line.Value, worktreeValueMaxBytes)})
		}
		view.Text, view.Cut = cutUTF8(result.Text, textMax)
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

// worktreeValueMaxBytes bounds one worktree line's value in a run's view.
const worktreeValueMaxBytes = 2048

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

// handleProfile answers a profile's info. Only session and profile are read; a profile the
// token's globs do not reach answers like one that does not exist.
func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request, token Token) {
	backend, ok := s.opts.Backend.(ProfileBackend)
	if !ok || !token.Allows(ActionProfile) {
		s.deny(w, token, ActionProfile, "profile")
		return
	}
	for key := range r.URL.Query() {
		if key != "session" && key != "profile" {
			writeError(w, http.StatusBadRequest, "bad_request", key+" is not a profile key")
			return
		}
	}
	session, profile := r.URL.Query().Get("session"), r.URL.Query().Get("profile")
	if !sessionPattern.MatchString(session) || profile == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session and profile are required")
		return
	}
	_, reference, inside := s.qualify(token, session)
	if !inside {
		writeError(w, http.StatusNotFound, "no_session", "that session does not exist on this server")
		return
	}
	mayProfile, _ := s.may(token)
	output, err := backend.ProfileInfo(r.Context(), ProfileRequest{Session: reference, Profile: profile, MayProfile: mayProfile})
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
	if path, ok := arguments["path"].(string); ok {
		qualified, inside := s.qualifyPath(token, path)
		if !inside {
			writeError(w, http.StatusNotFound, "no_session", "that session does not exist on this server")
			return
		}
		arguments["path"] = qualified
	}
	mayProfile, mayAgent := s.may(token)
	output, err := backend.Transcript(r.Context(), TranscriptRequest{Tool: tool, Arguments: arguments, MayProfile: mayProfile, MayAgent: mayAgent})
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
