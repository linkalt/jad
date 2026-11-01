package remoteapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

const (
	secretA = "tokenA_0123456789abcdef0123456789abcdef"
	secretB = "tokenB_0123456789abcdef0123456789abcdef"
)

type fakeRun struct {
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	result  RunResult
	stopped []string
}

func newFakeRun() *fakeRun               { return &fakeRun{done: make(chan struct{})} }
func (r *fakeRun) Done() <-chan struct{} { return r.done }
func (r *fakeRun) Result() RunResult     { r.mu.Lock(); defer r.mu.Unlock(); return r.result }
func (r *fakeRun) finish(result RunResult) {
	r.once.Do(func() { r.mu.Lock(); r.result = result; r.mu.Unlock(); close(r.done) })
}
func (r *fakeRun) Stop(reason string) {
	r.mu.Lock()
	r.stopped = append(r.stopped, reason)
	r.mu.Unlock()
	r.finish(RunResult{State: "stopped", Reason: reason})
}

type fakeBackend struct {
	mu       sync.Mutex
	messages []MessageRequest
	subs     []SubagentRequest
	runs     []*fakeRun
	profiles map[string][]string
	startErr error
	delay    chan struct{}
}

func (b *fakeBackend) Profiles(_ context.Context, session string) ([]string, error) {
	if names, ok := b.profiles[session]; ok {
		return names, nil
	}
	return nil, ErrNoSession
}
func (b *fakeBackend) start() (Run, error) {
	if b.delay != nil {
		<-b.delay
	}
	if b.startErr != nil {
		return nil, b.startErr
	}
	run := newFakeRun()
	b.mu.Lock()
	b.runs = append(b.runs, run)
	b.mu.Unlock()
	return run, nil
}
func (b *fakeBackend) SendMessage(_ context.Context, r MessageRequest) (Run, error) {
	b.mu.Lock()
	b.messages = append(b.messages, r)
	b.mu.Unlock()
	return b.start()
}
func (b *fakeBackend) StartSubagent(_ context.Context, r SubagentRequest) (Run, error) {
	b.mu.Lock()
	b.subs = append(b.subs, r)
	b.mu.Unlock()
	return b.start()
}

func tokens(t *testing.T, rows string) *TokenTable {
	t.Helper()
	parsed, err := ParseTokens([]byte(rows))
	if err != nil {
		t.Fatal(err)
	}
	return StaticTokenTable(parsed, nil)
}

func defaultTable(t *testing.T) *TokenTable {
	return tokens(t, fmt.Sprintf(`{"tokens":[
 {"id":"build","sha256":%q,"profiles":["main/review/*"],"agents":["main/review/*/explore"],"actions":["message","subagent","status","stop","transcripts","profile"]},
 {"id":"readonly","sha256":%q,"profiles":["main/*/*"],"actions":["status"]}]}`, hashOf(secretA), hashOf(secretB)))
}

type fixture struct {
	t       *testing.T
	backend *fakeBackend
	server  *Server
	http    *httptest.Server
}

func newFixture(t *testing.T, table *TokenTable, mutate func(*Options)) *fixture {
	t.Helper()
	backend := &fakeBackend{profiles: map[string][]string{"main": {"review/a", "review/b", "dev/coder"}}}
	opts := Options{Tokens: table, Backend: backend}
	if mutate != nil {
		mutate(&opts)
	}
	server, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() { httpServer.Close(); server.Close() })
	return &fixture{t: t, backend: backend, server: server, http: httpServer}
}

// raw is do for a test that reads the response headers as well.
func (f *fixture) raw(method, path, secret string) (int, http.Header, map[string]any) {
	f.t.Helper()
	request, _ := http.NewRequest(method, f.http.URL+path, bytes.NewReader(nil))
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(response.Body).Decode(&out)
	return response.StatusCode, response.Header, out
}

func (f *fixture) do(method, path, secret string, body any, headers ...string) (int, map[string]any) {
	f.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, _ := http.NewRequest(method, f.http.URL+path, reader)
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(response.Body).Decode(&out)
	return response.StatusCode, out
}

func message(session, profile, id string) map[string]any {
	return map[string]any{"session": session, "profile": profile, "message": "hello", "request_id": id}
}

func TestAuthRejectsMissingWrongRevokedAndExpiredTokens(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	table := tokens(t, fmt.Sprintf(`{"tokens":[
 {"id":"ok","sha256":%q,"profiles":["*/*/*"],"actions":["message"]},
 {"id":"revoked","sha256":%q,"revoked":true,"profiles":["*/*/*"],"actions":["message"]},
 {"id":"old","sha256":%q,"expires":"2025-12-31T00:00:00Z","profiles":["*/*/*"],"actions":["message"]}]}`,
		hashOf(secretA), hashOf(secretB), hashOf("tokenC_0123456789abcdef0123456789abcdef")))
	table.now = func() time.Time { return now }
	f := newFixture(t, table, nil)
	for name, secret := range map[string]string{"none": "", "wrong": "nope_0123456789abcdef0123456789abcdef", "revoked": secretB, "expired": "tokenC_0123456789abcdef0123456789abcdef"} {
		status, body := f.do("POST", Prefix+"/messages", secret, message("main", "review/a", "req-"+name))
		if status != 401 || body["code"] != "unauthorized" {
			t.Fatalf("%s: %d %v", name, status, body)
		}
	}
	if status, _ := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-ok-1")); status != 202 {
		t.Fatalf("a valid token must pass, got %d", status)
	}
	if len(f.backend.messages) != 1 {
		t.Fatalf("only the valid request may reach the backend: %d", len(f.backend.messages))
	}
}

func TestUIRoutesAndOtherPrefixesAreNotServed(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	for _, path := range []string{"/api/sessions", "/api/catalog/profile", "/api/remote/v2/profiles", "/api/remote/v1x", "/"} {
		if status, _ := f.do("GET", path, secretA, nil); status != 404 {
			t.Fatalf("%s: %d", path, status)
		}
	}
}

func TestActionAndGlobScope(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	cases := []struct {
		name, method, path, secret string
		body                       any
		want                       int
	}{
		{"message in scope", "POST", "/messages", secretA, message("main", "review/a", "req-scope-1"), 202},
		{"profile outside glob", "POST", "/messages", secretA, message("main", "dev/coder", "req-scope-2"), 403},
		{"session outside glob", "POST", "/messages", secretA, message("other", "review/a", "req-scope-3"), 403},
		{"action not granted", "POST", "/messages", secretB, message("main", "review/a", "req-scope-4"), 403},
		{"agent in scope", "POST", "/subagents", secretA, map[string]any{"session": "main", "profile": "review/a", "agent": "explore", "task": "t", "request_id": "req-scope-5"}, 202},
		{"agent outside glob", "POST", "/subagents", secretA, map[string]any{"session": "main", "profile": "review/a", "agent": "other", "task": "t", "request_id": "req-scope-6"}, 403},
		{"agent uses profile glob?", "POST", "/subagents", secretB, map[string]any{"session": "main", "profile": "dev/coder", "agent": "explore", "task": "t", "request_id": "req-scope-7"}, 403},
		{"glob star does not cross slash", "POST", "/messages", secretA, message("main", "review/a/deep", "req-scope-8"), 400},
	}
	for _, c := range cases {
		if status, body := f.do(c.method, Prefix+c.path, c.secret, c.body); status != c.want {
			t.Fatalf("%s: got %d want %d (%v)", c.name, status, c.want, body)
		}
	}
	for _, request := range f.backend.messages {
		if request.Profile != "review/a" || request.Caller != "remote:build" {
			t.Fatalf("backend saw %+v", request)
		}
	}
	if len(f.backend.subs) != 1 || f.backend.subs[0].Profile != "review/a" || f.backend.subs[0].Agent != "explore" {
		t.Fatalf("subagent request: %+v", f.backend.subs)
	}
}

func TestProfilesListIsFilteredByTheToken(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	status, body := f.do("GET", Prefix+"/profiles?session=main", secretA, nil)
	names, _ := body["profiles"].([]any)
	if status != 200 || len(names) != 2 || names[0] != "review/a" || names[1] != "review/b" {
		t.Fatalf("filtered profiles: %d %v", status, body)
	}
	if status, body := f.do("GET", Prefix+"/profiles?session=nope", secretA, nil); status != 404 || body["code"] != "no_session" {
		t.Fatalf("missing session must not be created or guessed: %d %v", status, body)
	}
	// The token sees none of side's profiles: the answer must match a missing session.
	f.backend.profiles["side"] = []string{"dev/coder"}
	missing, missingBody := f.do("GET", Prefix+"/profiles?session=nope", secretA, nil)
	hidden, hiddenBody := f.do("GET", Prefix+"/profiles?session=side", secretA, nil)
	if hidden != missing || hiddenBody["code"] != missingBody["code"] {
		t.Fatalf("an inaccessible session must answer like a missing one: %d %v, %d %v", hidden, hiddenBody, missing, missingBody)
	}
	if status, _ := f.do("GET", Prefix+"/profiles?session=main", secretB, nil); status != 403 {
		t.Fatalf("a token with no message/subagent/transcripts action: %d", status)
	}
}

func TestDuplicateRequestIDStartsOnce(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	status1, first := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-dup-0001"))
	status2, second := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-dup-0001"))
	if status1 != 202 || status2 != 200 || first["id"] != second["id"] {
		t.Fatalf("repeat: %d %v / %d %v", status1, first, status2, second)
	}
	if len(f.backend.messages) != 1 {
		t.Fatalf("the message was started %d times", len(f.backend.messages))
	}
}

func TestConcurrentDuplicatesStartOnce(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(o *Options) {})
	f.backend.delay = make(chan struct{})
	var wg sync.WaitGroup
	ids := make([]any, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, body := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-race-0001"))
			ids[i] = body["id"]
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(f.backend.delay)
	wg.Wait()
	for _, id := range ids {
		if id == nil || id != ids[0] {
			t.Fatalf("ids differ: %v", ids)
		}
	}
	if len(f.backend.messages) != 1 {
		t.Fatalf("started %d times", len(f.backend.messages))
	}
}

func TestRequestIDsAreScopedToTheToken(t *testing.T) {
	table := tokens(t, fmt.Sprintf(`{"tokens":[
 {"id":"a","sha256":%q,"profiles":["*/*/*"],"actions":["message","status","stop"]},
 {"id":"b","sha256":%q,"profiles":["*/*/*"],"actions":["message","status","stop"]}]}`, hashOf(secretA), hashOf(secretB)))
	f := newFixture(t, table, nil)
	_, one := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-same-0001"))
	_, two := f.do("POST", Prefix+"/messages", secretB, message("main", "review/a", "req-same-0001"))
	if one["id"] == two["id"] || len(f.backend.messages) != 2 {
		t.Fatal("two tokens sharing a request id must not share a run")
	}
	id, _ := one["id"].(string)
	if status, _ := f.do("GET", Prefix+"/runs/"+id, secretB, nil); status != 404 {
		t.Fatalf("a token must not see another token's run: %d", status)
	}
	if status, _ := f.do("POST", Prefix+"/runs/"+id+"/stop", secretB, nil); status != 404 {
		t.Fatalf("a token must not stop another token's run: %d", status)
	}
	if len(f.backend.runs[0].stopped) != 0 {
		t.Fatal("run was stopped by a foreign token")
	}
}

func TestStatusWaitAndStop(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	_, started := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-wait-0001"))
	id := started["id"].(string)
	begin := time.Now()
	status, body := f.do("GET", Prefix+"/runs/"+id+"?wait_ms=80", secretA, nil)
	if status != 200 || body["state"] != "running" || time.Since(begin) < 70*time.Millisecond {
		t.Fatalf("wait on a running run: %d %v after %v", status, body, time.Since(begin))
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.backend.runs[0].finish(RunResult{State: "done", Text: "the answer"})
	}()
	begin = time.Now()
	status, body = f.do("GET", Prefix+"/runs/"+id+"?wait_ms=5000", secretA, nil)
	if status != 200 || body["state"] != "done" || body["text"] != "the answer" || time.Since(begin) > 2*time.Second {
		t.Fatalf("wait must return when the run ends: %d %v", status, body)
	}
	_, second := f.do("POST", Prefix+"/messages", secretA, message("main", "review/b", "req-stop-0001"))
	id2 := second["id"].(string)
	status, body = f.do("POST", Prefix+"/runs/"+id2+"/stop", secretA, nil)
	if status != 200 || body["state"] != "stopped" {
		t.Fatalf("stop: %d %v", status, body)
	}
	if status, body = f.do("POST", Prefix+"/runs/"+id2+"/stop", secretA, nil); status != 200 || body["state"] != "stopped" {
		t.Fatalf("stop must be idempotent: %d %v", status, body)
	}
}

func TestWaitIsCappedAndNeedsStatusAction(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(o *Options) { o.WaitMax = 60 * time.Millisecond })
	_, started := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-cap-00001"))
	begin := time.Now()
	f.do("GET", Prefix+"/runs/"+started["id"].(string)+"?wait_ms=600000", secretA, nil)
	if time.Since(begin) > 2*time.Second {
		t.Fatal("wait_ms must be capped by the server")
	}
	if status, _ := f.do("GET", Prefix+"/runs/"+started["id"].(string)+"?wait_ms=-1", secretA, nil); status != 400 {
		t.Fatalf("negative wait: %d", status)
	}
	if status, _ := f.do("GET", Prefix+"/runs/rr_nothexnothexnoth", secretA, nil); status != 404 {
		t.Fatalf("bad run id: %d", status)
	}
}

func TestHopLimit(t *testing.T) {
	table := tokens(t, fmt.Sprintf(`{"tokens":[
 {"id":"strict","sha256":%q,"profiles":["*/*/*"],"actions":["message"],"max_hops":1},
 {"id":"loose","sha256":%q,"profiles":["*/*/*"],"actions":["message"],"max_hops":99}]}`, hashOf(secretA), hashOf(secretB)))
	f := newFixture(t, table, nil)
	for _, c := range []struct {
		secret, hops string
		want         int
	}{{secretB, "", 202}, {secretB, "3", 202}, {secretB, "4", 508}, {secretA, "1", 202}, {secretA, "2", 508}, {secretB, "x", 400}, {secretB, "-1", 400}} {
		status, body := f.do("POST", Prefix+"/messages", c.secret, message("main", "review/a", fmt.Sprintf("req-hop-%s-%s", c.secret[:6], c.hops)), HeaderHops, c.hops)
		_ = body
		want := c.want
		if want == 508 {
			want = http.StatusLoopDetected
		}
		if status != want {
			t.Fatalf("hops %q with %s: got %d want %d (%v)", c.hops, c.secret[:6], status, want, body)
		}
	}
	for _, request := range f.backend.messages {
		if request.Hops > 3 {
			t.Fatalf("a request past the limit reached the backend: %+v", request)
		}
	}
}

func TestHopLimitCheckedBeforeAnyWork(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(o *Options) { o.MaxHops = 2 })
	status, body := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-hop-0001"), HeaderHops, "3")
	if status != http.StatusLoopDetected || body["code"] != "remote_hop_limit" || len(f.backend.messages) != 0 {
		t.Fatalf("%d %v", status, body)
	}
}

func TestBackendErrorsAreMappedAndNeverLeaked(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	for _, c := range []struct {
		err  error
		want int
		code string
	}{
		{ErrUnknownTarget, 404, "unknown_target"},
		{ErrNoSession, 404, "no_session"},
		{ErrBusy, 409, "profile_busy"},
		{&Refusal{Code: "profile_cycle", Message: "it would form a cycle"}, 409, "profile_cycle"},
		{errors.New("open /secret/path/state.json: permission denied"), 500, "internal"},
	} {
		f.backend.startErr = c.err
		status, body := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-err-"+c.code))
		if status != c.want || body["code"] != c.code {
			t.Fatalf("%v: %d %v", c.err, status, body)
		}
		if strings.Contains(fmt.Sprint(body), "/secret/path") {
			t.Fatalf("internal text leaked: %v", body)
		}
	}
	f.backend.startErr = nil
	if status, _ := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-err-ErrBusy")); status != 202 {
		t.Fatalf("a failed start must not be remembered under its request id: %d", status)
	}
}

func TestRequestValidationAndBodyCap(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(o *Options) { o.BodyMaxBytes = 256 })
	for name, body := range map[string]any{
		"no request id": map[string]any{"session": "main", "profile": "review/a", "message": "x"},
		"short request": message("main", "review/a", "abc"),
		"bad session":   message("../x", "review/a", "req-val-0001"),
		"no category":   message("main", "a", "req-val-0002"),
		"empty message": map[string]any{"session": "main", "profile": "review/a", "message": "  ", "request_id": "req-val-0003"},
		"unknown key":   map[string]any{"session": "main", "profile": "review/a", "message": "x", "request_id": "req-val-0004", "model": "y"},
	} {
		if status, _ := f.do("POST", Prefix+"/messages", secretA, body); status != 400 {
			t.Fatalf("%s: %d", name, status)
		}
	}
	big := message("main", "review/a", "req-val-0005")
	big["message"] = strings.Repeat("x", 1000)
	if status, _ := f.do("POST", Prefix+"/messages", secretA, big); status != 413 {
		t.Fatalf("body cap: %d", status)
	}
	if len(f.backend.messages) != 0 {
		t.Fatal("an invalid request reached the backend")
	}
}

func TestResultIsCutAtResultMaxBytes(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(o *Options) { o.ResultMaxBytes = 10 })
	_, started := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-cut-00001"))
	f.backend.runs[0].finish(RunResult{State: "done", Text: "ééééééééééé"})
	_, body := f.do("GET", Prefix+"/runs/"+started["id"].(string), secretA, nil)
	text, _ := body["text"].(string)
	if len(text) > 10 || body["cut"] != true || strings.ContainsRune(text, '\uFFFD') {
		t.Fatalf("cut: %q %v", text, body)
	}
}

func TestRunLimitAndRetention(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	f := newFixture(t, defaultTable(t), func(o *Options) {
		o.MaxRuns = 2
		o.RunRetention = time.Minute
		o.Now = func() time.Time { return now.Add(time.Duration(clock.Load())) }
	})
	f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-lim-00001"))
	f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-lim-00002"))
	if status, body := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-lim-00003")); status != 429 || body["code"] != "too_many_runs" {
		t.Fatalf("run cap: %d %v", status, body)
	}
	f.backend.runs[0].finish(RunResult{State: "done", Text: "x"})
	time.Sleep(20 * time.Millisecond)
	clock.Store(int64(2 * time.Minute))
	if status, _ := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-lim-00003")); status != 202 {
		t.Fatalf("a finished run past retention must free its place: %d", status)
	}
}

func TestOneTokenCannotFillTheServerWithFinishedOrLiveRuns(t *testing.T) {
	table := tokens(t, fmt.Sprintf(`{"tokens":[
 {"id":"a","sha256":%q,"profiles":["main/*/*"],"actions":["message","status"]},
 {"id":"b","sha256":%q,"profiles":["main/*/*"],"actions":["message","status"]}]}`, hashOf(secretA), hashOf(secretB)))
	f := newFixture(t, table, func(o *Options) { o.MaxRuns = 4; o.MaxRunsPerToken = 2 })
	post := func(secret, id string) int {
		status, _ := f.do("POST", Prefix+"/messages", secret, message("main", "review/a", id))
		return status
	}
	for i := 1; i <= 2; i++ {
		if status := post(secretA, fmt.Sprintf("req-live-%04d", i)); status != 202 {
			t.Fatalf("live run %d: %d", i, status)
		}
	}
	if status := post(secretA, "req-live-0003"); status != 429 {
		t.Fatalf("a token over its live cap: %d", status)
	}
	f.backend.runs[0].finish(RunResult{State: "done"})
	f.backend.runs[1].finish(RunResult{State: "done"})
	time.Sleep(20 * time.Millisecond)
	for i := 3; i <= 6; i++ {
		if status := post(secretA, fmt.Sprintf("req-fin-%04d", i)); status != 202 {
			t.Fatalf("finished runs must not count against the live cap: run %d got %d", i, status)
			return
		}
		f.backend.runs[len(f.backend.runs)-1].finish(RunResult{State: "done"})
		time.Sleep(20 * time.Millisecond)
	}
	if status := post(secretB, "req-other-0001"); status != 202 {
		t.Fatalf("another token must still get a slot when the table is full of finished runs: %d", status)
	}
}

func TestReusedRequestIDWithOtherContentIsRefused(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	if status, _ := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-same-0001")); status != 202 {
		t.Fatal(status)
	}
	other := message("main", "review/b", "req-same-0001")
	if status, body := f.do("POST", Prefix+"/messages", secretA, other); status != 409 || body["code"] != "request_id_reused" {
		t.Fatalf("other profile: %d %v", status, body)
	}
	other = message("main", "review/a", "req-same-0001")
	other["message"] = "something else"
	if status, body := f.do("POST", Prefix+"/messages", secretA, other); status != 409 || body["code"] != "request_id_reused" {
		t.Fatalf("other text: %d %v", status, body)
	}
	if status, _ := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-same-0001")); status != 200 {
		t.Fatalf("the identical repeat is still the same run: %d", status)
	}
	if len(f.backend.messages) != 1 {
		t.Fatalf("started %d times", len(f.backend.messages))
	}
}

func TestCloseDuringStartStopsTheRunThatWasStarting(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	f.backend.delay = make(chan struct{})
	done := make(chan int, 1)
	go func() {
		status, _ := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-race-0001"))
		done <- status
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.backend.mu.Lock()
		started := len(f.backend.messages)
		f.backend.mu.Unlock()
		if started == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	f.server.Close()
	close(f.backend.delay)
	<-done
	f.backend.mu.Lock()
	run := f.backend.runs[0]
	f.backend.mu.Unlock()
	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a run that finished starting after Close was left running")
	}
}

// A run ID that cannot be random is refused, not minted from zero bytes, so no two runs can share an ID.
func TestRunIDIsRefusedWhenTheRandomSourceFails(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(opts *Options) {
		opts.Random = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	})
	status, body := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-entropy-0001"))
	if status != http.StatusServiceUnavailable || body["code"] != "unavailable" {
		t.Fatalf("run with no entropy: status %d body %v, want 503 unavailable", status, body)
	}
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	if len(f.server.runs) != 0 {
		t.Fatalf("a run was registered without an ID: %v", f.server.runs)
	}
}

func TestCloseStopsLiveRuns(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-close-0001"))
	f.server.Close()
	run := f.backend.runs[0]
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.stopped) != 1 || run.stopped[0] != "server_shutdown" {
		t.Fatalf("stopped: %v", run.stopped)
	}
	if status, _ := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-close-0002")); status != 503 {
		t.Fatalf("closed server: %d", status)
	}
}

func TestTokenTableValidation(t *testing.T) {
	good := hashOf(secretA)
	for name, c := range map[string]struct{ body, want string }{
		"unknown key":   {`{"tokens":[{"id":"a","sha256":"` + good + `","actions":[],"secret":"x"}]}`, "unknown field"},
		"plaintext":     {`{"tokens":[{"id":"a","sha256":"` + secretA + `"}]}`, "64 hex"},
		"bad action":    {`{"tokens":[{"id":"a","sha256":"` + good + `","actions":["delete"]}]}`, "unknown action"},
		"dup id":        {`{"tokens":[{"id":"a","sha256":"` + good + `"},{"id":"a","sha256":"` + hashOf(secretB) + `"}]}`, "unique"},
		"dup hash":      {`{"tokens":[{"id":"a","sha256":"` + good + `"},{"id":"b","sha256":"` + good + `"}]}`, "unique"},
		"bad glob":      {`{"tokens":[{"id":"a","sha256":"` + good + `","profiles":["[x"]}]}`, "valid glob"},
		"bad expires":   {`{"tokens":[{"id":"a","sha256":"` + good + `","expires":"tomorrow"}]}`, "RFC 3339"},
		"negative hops": {`{"tokens":[{"id":"a","sha256":"` + good + `","max_hops":-1}]}`, "negative"},
		"bad id":        {`{"tokens":[{"id":"a b","sha256":"` + good + `"}]}`, "id must be"},
	} {
		if _, err := ParseTokens([]byte(c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: want %q got %v", name, c.want, err)
		}
	}
}

func TestTokenFileReloadRevokesAtOnce(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tokens.json")
	write := func(body string) {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Hour)
		_ = os.Chtimes(file, future, future)
	}
	row := func(revoked bool) string {
		return fmt.Sprintf(`{"tokens":[{"id":"a","sha256":%q,"revoked":%v,"profiles":["*/*/*"],"actions":["message"]}]}`, hashOf(secretA), revoked)
	}
	write(row(false))
	table, err := NewTokenTable(file, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	table.interval = 0
	if _, err := table.Authenticate("Bearer " + secretA); err != nil {
		t.Fatal(err)
	}
	write(strings.Replace(row(true), "true", "true", 1) + " ")
	if _, err := table.Authenticate("Bearer " + secretA); err == nil {
		t.Fatal("a token revoked in the file must be refused on the next request")
	}
	write(`{broken`)
	if _, err := table.Authenticate("Bearer " + secretA); err == nil {
		t.Fatal("a broken table must refuse everything, not keep old grants")
	}
	write(row(false) + "  ")
	if _, err := table.Authenticate("Bearer " + secretA); err != nil {
		t.Fatalf("a repaired table must work again: %v", err)
	}
	if _, err := NewTokenTable(filepath.Join(dir, "missing.json"), nil, nil); err == nil {
		t.Fatal("a missing table must fail at start")
	}
}

type transcriptFake struct {
	*fakeBackend
	got []TranscriptRequest
}

func (b *transcriptFake) Transcript(_ context.Context, r TranscriptRequest) (string, error) {
	b.got = append(b.got, r)
	return "rendered", nil
}

func TestTranscriptRouteNeedsTheActionAndABackendThatServesIt(t *testing.T) {
	plain := newFixture(t, defaultTable(t), nil)
	if status, _ := plain.do("GET", Prefix+"/transcripts?path=session:main", secretA, nil); status != http.StatusForbidden {
		t.Fatalf("a backend without transcripts: %d", status)
	}
	backend := &transcriptFake{fakeBackend: &fakeBackend{profiles: map[string][]string{"main": {"review/a"}}}}
	f := newFixture(t, defaultTable(t), func(o *Options) { o.Backend = backend })
	if status, _ := f.do("GET", Prefix+"/transcripts?path=session:main", secretB, nil); status != http.StatusForbidden || len(backend.got) != 0 {
		t.Fatalf("a token without the transcripts action: %d", status)
	}
	status, body := f.do("GET", Prefix+"/transcripts?path=session:main&filters=a&filters=b&limit=3", secretA, nil)
	if status != 200 || body["output"] != "rendered" || len(backend.got) != 1 {
		t.Fatalf("%d %v", status, body)
	}
	got := backend.got[0]
	if got.Tool != "transcripts" || got.Arguments["limit"] != json.Number("3") || len(got.Arguments["filters"].([]any)) != 2 || !got.MayProfile("main/review/a") || got.MayProfile("main/dev/coder") {
		t.Fatalf("%+v", got)
	}
	for _, query := range []string{"/transcript?bogus=1", "/transcripts?limit=-1", "/transcripts?path=", "/transcripts?path=a&path=b", "/transcript?search=%00"} {
		if status, _ := f.do("GET", Prefix+query, secretA, nil); status != http.StatusBadRequest {
			t.Errorf("%s: %d", query, status)
		}
	}
}

func TestTokenFileMustBePrivateAndChangesAreSeenByContent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tokens.json")
	row := func(revoked string) string {
		return fmt.Sprintf(`{"tokens":[{"id":"a","sha256":%q,"revoked":%s,"profiles":["*/*/*"],"actions":["message"]}]}`, hashOf(secretA), revoked)
	}
	if err := os.WriteFile(file, []byte(row("false")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTokenTable(file, nil, nil); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a table open to others must not load: %v", err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := NewTokenTable(file, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	table.interval = 0
	if _, err := table.Authenticate("Bearer " + secretA); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(file)
	if err := os.WriteFile(file, []byte(row("true ")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(file); after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		t.Fatalf("the test needs the same size and mtime: %d/%d", after.Size(), info.Size())
	}
	if _, err := table.Authenticate("Bearer " + secretA); err == nil {
		t.Fatal("a same-size, same-mtime rewrite that revokes a token must be seen")
	}
	if err := os.WriteFile(file, []byte(row("false")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Authenticate("Bearer " + secretA); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Authenticate("Bearer " + secretA); err == nil {
		t.Fatal("a table that became open to others must refuse every request")
	}
}

// A token table that cannot be read is the server's fault, not the caller's: the answer is
// 503 without a challenge, so a client that is not refused for its own token retries and a
// followed run is not ended. An unknown token keeps 401 and the challenge.
func TestBrokenTokenTableAnswers503AndAnUnknownTokenStays401(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tokens.json")
	body := fmt.Sprintf(`{"tokens":[{"id":"a","sha256":%q,"profiles":["*/*/*"],"actions":["message"]}]}`, hashOf(secretA))
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := NewTokenTable(file, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	table.interval = 0
	f := newFixture(t, table, nil)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	status, headers, body2 := f.raw("POST", Prefix+"/messages", secretA)
	if status != http.StatusServiceUnavailable || body2["code"] != "token_table_unusable" {
		t.Fatalf("broken table: status %d body %v, want 503 token_table_unusable", status, body2)
	}
	if headers.Get("WWW-Authenticate") != "" {
		t.Fatalf("broken table sent WWW-Authenticate %q, want none", headers.Get("WWW-Authenticate"))
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	status, headers, _ = f.raw("POST", Prefix+"/messages", "unknown_0123456789abcdef0123456789abcdef")
	if status != http.StatusUnauthorized || headers.Get("WWW-Authenticate") == "" {
		t.Fatalf("unknown token: status %d WWW-Authenticate %q, want 401 with the challenge", status, headers.Get("WWW-Authenticate"))
	}
}

func TestTokenFileIsLookedAtOncePerInterval(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tokens.json")
	body := fmt.Sprintf(`{"tokens":[{"id":"a","sha256":%q,"profiles":["*/*/*"],"actions":["message"]}]}`, hashOf(secretA))
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := NewTokenTable(file, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	table.interval = time.Hour
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Authenticate("Bearer " + secretA); err != nil {
		t.Fatalf("inside the interval the table is not read again: %v", err)
	}
	table.interval = 0
	if _, err := table.Authenticate("Bearer " + secretA); err == nil {
		t.Fatal("a vanished table must refuse every request once it is looked at")
	}
}

func TestTokenFileReloadAndAuthenticateRace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tokens.json")
	row := func(revoked bool) []byte {
		return []byte(fmt.Sprintf(`{"tokens":[{"id":"a","sha256":%q,"revoked":%v,"profiles":["*/*/*"],"actions":["message"]}]}`, hashOf(secretA), revoked))
	}
	if err := os.WriteFile(file, row(false), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := NewTokenTable(file, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	table.interval = 0
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_, _ = table.Authenticate("Bearer " + secretA)
			}
		}()
	}
	for i := range 50 {
		tmp := file + ".new"
		_ = os.WriteFile(tmp, row(i%2 == 0), 0o600)
		_ = os.Rename(tmp, file)
	}
	wg.Wait()
}

func TestGlobsWithCharacterClassesAreRejected(t *testing.T) {
	for _, glob := range []string{"main[^x]*", "main/[a-z]*/x", "ma[i]n/*/*"} {
		for _, key := range []string{"profiles", "agents"} {
			body := fmt.Sprintf(`{"tokens":[{"id":"a","sha256":%q,%q:[%q],"actions":["message"]}]}`, hashOf(secretA), key, glob)
			if _, err := ParseTokens([]byte(body)); err == nil {
				t.Errorf("%s %q must be refused: a class can match '/'", key, glob)
			}
		}
	}
}
