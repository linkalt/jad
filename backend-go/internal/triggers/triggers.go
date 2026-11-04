// Package triggers starts runs without a client: on a cron schedule, or when a signed
// webhook arrives (a CI failure, a new issue). Each run goes through the same in-process
// run route as "jad run" (package headless), so it takes the session's locks, writes the
// same messages.jsonl and shows up in the web client like any other turn.
package triggers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs"
)

// Sources of a trigger run.
const (
	SourceSchedule = "schedule"
	SourceWebhook  = "webhook"
)

// Statuses a trigger run ends with besides the headless ones (completed, failed, aborted,
// timed_out): refused when the run route did not start it (unknown session, a run already
// active), skipped when the trigger's previous run had not ended.
const (
	StatusRunning = "running"
	StatusRefused = "refused"
	StatusSkipped = "skipped"
)

// ErrBusy is returned by Fire when the trigger's previous run has not ended.
var ErrBusy = errors.New("the trigger's previous run has not ended")

// Runner runs one turn; *headless.Runner is the production one.
type Runner interface {
	Run(ctx context.Context, request headless.Request) (headless.Result, error)
}

// Options configure a Manager. Now and After replace the clock in tests.
type Options struct {
	Settings    sessionfs.TriggerSettings
	Environment map[string]string
	Runner      Runner
	// APIToken guards GET /api/triggers, as it guards the rest of the API.
	APIToken string
	Logger   *slog.Logger
	Now      func() time.Time
	After    func(time.Duration) <-chan time.Time
}

// Record is one trigger run, as GET /api/triggers lists it.
type Record struct {
	ID         string    `json:"id"`
	Trigger    string    `json:"trigger"`
	Source     string    `json:"source"`
	Session    string    `json:"session"`
	Profile    string    `json:"profile,omitempty"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitzero"`
	TurnID     string    `json:"turn_id,omitempty"`
	Error      string    `json:"error,omitempty"`
	CostUSD    *float64  `json:"cost_usd,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
}

// Manager owns the schedules, the webhook route and the history of trigger runs.
type Manager struct {
	settings sessionfs.TriggerSettings
	entries  map[string]sessionfs.TriggerEntry
	secrets  map[string][]byte
	runner   Runner
	token    string
	logger   *slog.Logger
	now      func() time.Time
	after    func(time.Duration) <-chan time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	running map[string]*Record
	history []Record
	started bool
	closed  bool
}

// New checks the settings against the environment: every webhook's secret variable must be
// set and non-empty, so no webhook ever accepts unsigned requests.
func New(options Options) (*Manager, error) {
	if options.Runner == nil {
		return nil, errors.New("server.triggers: no runner")
	}
	manager := &Manager{
		settings: options.Settings, entries: map[string]sessionfs.TriggerEntry{}, secrets: map[string][]byte{},
		runner: options.Runner, token: options.APIToken, logger: options.Logger, now: options.Now, after: options.After,
		running: map[string]*Record{},
	}
	if manager.logger == nil {
		manager.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if manager.now == nil {
		manager.now = time.Now
	}
	if manager.after == nil {
		manager.after = time.After
	}
	for _, entry := range options.Settings.Entries {
		manager.entries[entry.Name] = entry
		if entry.WebhookSecretEnv == "" {
			continue
		}
		secret := options.Environment[entry.WebhookSecretEnv]
		if secret == "" {
			return nil, fmt.Errorf("server.triggers.entries.%s.webhook.secret_env: the environment variable %s is not set or empty", entry.Name, entry.WebhookSecretEnv)
		}
		manager.secrets[entry.Name] = []byte(secret)
	}
	manager.ctx, manager.cancel = context.WithCancel(context.Background())
	return manager, nil
}

// Start starts one scheduler per trigger with a schedule. It is called once.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true
	for _, entry := range m.settings.Entries {
		if entry.Cron == nil {
			continue
		}
		m.wg.Add(1)
		go m.schedule(entry)
	}
}

// Close stops the schedulers, stops the runs in progress and waits for them to end.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) schedule(entry sessionfs.TriggerEntry) {
	defer m.wg.Done()
	for {
		now := m.now()
		next := entry.Cron.Next(now.In(entry.Location))
		if next.IsZero() {
			m.logger.Warn("trigger schedule never matches again", slog.String("trigger", entry.Name), slog.String("schedule", entry.Schedule))
			return
		}
		select {
		case <-m.ctx.Done():
			return
		case <-m.after(next.Sub(now)):
		}
		if _, err := m.Fire(entry.Name, SourceSchedule, nil); err != nil && !errors.Is(err, ErrBusy) {
			m.logger.Warn("trigger did not start", slog.String("trigger", entry.Name), slog.Any("error", err))
		}
	}
}

// Fire starts the trigger's run in the background and returns its record. A trigger runs
// once at a time: while its previous run is in progress, Fire records a skipped run and
// returns ErrBusy.
func (m *Manager) Fire(name, source string, payload []byte) (Record, error) {
	entry, ok := m.entries[name]
	if !ok {
		return Record{}, fmt.Errorf("no trigger named %q", name)
	}
	now := m.now()
	record := Record{ID: newRunID(), Trigger: name, Source: source, Session: entry.Session, Profile: entry.Profile, Status: StatusRunning, StartedAt: now.UTC()}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Record{}, errors.New("the server is shutting down")
	}
	if previous := m.running[name]; previous != nil {
		record.Status, record.EndedAt, record.Error = StatusSkipped, now.UTC(), "run "+previous.ID+" has not ended"
		m.remember(record)
		m.mu.Unlock()
		m.logger.Info("trigger skipped", slog.String("trigger", name), slog.String("source", source), slog.String("running", previous.ID))
		return record, ErrBusy
	}
	running := record
	m.running[name] = &running
	m.wg.Add(1)
	m.mu.Unlock()

	input := strings.NewReplacer("{trigger}", name, "{time}", now.UTC().Format(time.RFC3339), "{payload}", string(payload)).Replace(entry.Prompt)
	m.logger.Info("trigger run started", slog.String("trigger", name), slog.String("source", source), slog.String("run", record.ID), slog.String("session", entry.Session))
	go m.run(entry, record, input)
	return record, nil
}

func (m *Manager) run(entry sessionfs.TriggerEntry, record Record, input string) {
	defer m.wg.Done()
	result, err := m.runner.Run(m.ctx, headless.Request{
		Session: entry.Session, Profile: entry.Profile, Model: entry.Model, Input: input,
		Timeout: time.Duration(entry.TimeoutMS) * time.Millisecond,
	})
	record.EndedAt = m.now().UTC()
	var refused *headless.RequestError
	switch {
	case errors.As(err, &refused):
		record.Status, record.Error = StatusRefused, refused.Message
	case err != nil:
		record.Status, record.Error = headless.StatusFailed, err.Error()
	default:
		record.Status, record.Error, record.TurnID, record.CostUSD, record.DurationMS = result.Status, result.Error, result.TurnID, result.CostUSD, result.DurationMS
	}
	m.mu.Lock()
	delete(m.running, entry.Name)
	m.remember(record)
	m.mu.Unlock()
	m.logger.Info("trigger run ended", slog.String("trigger", entry.Name), slog.String("run", record.ID), slog.String("status", record.Status), slog.String("error", record.Error))
}

// remember adds a finished record, dropping the oldest past the history limit. m.mu is held.
func (m *Manager) remember(record Record) {
	m.history = append(m.history, record)
	if excess := len(m.history) - int(m.settings.HistoryLimit); excess > 0 {
		m.history = append([]Record(nil), m.history[excess:]...)
	}
}

// Wrap serves /api/triggers in front of next: POST /api/triggers/<name> is a webhook,
// signed instead of carrying the API token; GET /api/triggers lists the triggers and their
// runs and needs the API token. Every other path goes to next.
func (m *Manager) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/triggers":
			m.serveList(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/triggers/"):
			m.serveWebhook(w, r, strings.TrimPrefix(r.URL.Path, "/api/triggers/"))
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (m *Manager) serveWebhook(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "a webhook takes POST")
		return
	}
	secret, ok := m.secrets[name]
	if !ok {
		writeError(w, http.StatusNotFound, "no webhook trigger named "+name)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, m.settings.MaxPayloadBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes (setting: server.triggers.max_payload_bytes)", m.settings.MaxPayloadBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "the body could not be read")
		return
	}
	if !validSignature(secret, body, r.Header.Get(m.settings.SignatureHeader)) {
		m.logger.Warn("trigger webhook with a bad signature", slog.String("trigger", name))
		writeError(w, http.StatusUnauthorized, "missing or wrong "+m.settings.SignatureHeader+" (HMAC-SHA256 of the body)")
		return
	}
	record, err := m.Fire(name, SourceWebhook, body)
	switch {
	case errors.Is(err, ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "run": record})
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"run": record})
	}
}

func (m *Manager) serveList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "the trigger list takes GET")
		return
	}
	given, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || m.token == "" || subtle.ConstantTimeCompare([]byte(given), []byte(m.token)) != 1 {
		writeError(w, http.StatusUnauthorized, "missing or wrong API token")
		return
	}
	type trigger struct {
		Name     string     `json:"name"`
		Session  string     `json:"session"`
		Profile  string     `json:"profile,omitempty"`
		Model    string     `json:"model,omitempty"`
		Schedule string     `json:"schedule,omitempty"`
		Timezone string     `json:"timezone,omitempty"`
		NextRun  *time.Time `json:"next_run,omitempty"`
		Webhook  bool       `json:"webhook"`
		Running  *Record    `json:"running,omitempty"`
	}
	now := m.now()
	list := make([]trigger, 0, len(m.settings.Entries))
	m.mu.Lock()
	for _, entry := range m.settings.Entries {
		item := trigger{Name: entry.Name, Session: entry.Session, Profile: entry.Profile, Model: entry.Model, Webhook: entry.WebhookSecretEnv != ""}
		if entry.Cron != nil {
			item.Schedule, item.Timezone = entry.Schedule, entry.Timezone
			if next := entry.Cron.Next(now.In(entry.Location)); !next.IsZero() {
				item.NextRun = &next
			}
		}
		if running := m.running[entry.Name]; running != nil {
			copied := *running
			item.Running = &copied
		}
		list = append(list, item)
	}
	runs := make([]Record, len(m.history))
	for index, record := range m.history {
		runs[len(runs)-1-index] = record
	}
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"triggers": list, "runs": runs})
}

// validSignature checks header, the hex HMAC-SHA256 of body with or without "sha256=".
func validSignature(secret, body []byte, header string) bool {
	given, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(header), "sha256="))
	if err != nil || len(given) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(given, mac.Sum(nil))
}

func newRunID() string {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return "trg_" + hex.EncodeToString(raw)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
