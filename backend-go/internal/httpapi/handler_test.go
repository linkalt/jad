package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/tools"
)

const testAPIToken = "0123456789abcdef0123456789abcdef"

func TestRunStartAttachStopRoutesAndFrontendEvents(t *testing.T) {
	streamedEvent := json.RawMessage(`{"type":"response.output_text.delta","delta":"hel"}`)
	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}`)
	runner := runnerFunc(func(_ context.Context, input json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		if string(input) != `"hello"` {
			t.Errorf("runner input = %s, want JSON string", input)
		}
		if options.OnEvent != nil {
			if err := options.OnEvent(responses.Event{Type: "response.output_text.delta", Raw: streamedEvent}); err != nil {
				return session.RunResult{}, err
			}
		}
		return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-1", Profile: "dev", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken:            testAPIToken,
		AllowedOrigins:      []string{"https://ui.example.test"},
		MaxBodyBytes:        4096,
		SSEMaxBufferedBytes: 4096,
		RunStateRetention:   time.Minute,
	})
	defer handler.Close()

	start := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/run?profile=dev", strings.NewReader(`{"input":"hello","model":"models/custom.json"}`))
	start.Header.Set("Authorization", "Bearer "+testAPIToken)
	start.Header.Set("Origin", "https://ui.example.test")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, start)
	if startResponse.Code != http.StatusOK {
		t.Fatalf("POST run status = %d, body=%s", startResponse.Code, startResponse.Body.String())
	}
	if got := startResponse.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Errorf("SSE content type = %q", got)
	}
	if got := startResponse.Header().Get("Access-Control-Allow-Origin"); got != "https://ui.example.test" {
		t.Errorf("CORS origin = %q", got)
	}
	startEvents := parseEvents(t, startResponse.Body.String())
	if got, want := eventNames(startEvents), []string{"run", "ui.message", "ui.message", "completed", "done"}; !equalStrings(got, want) {
		t.Fatalf("start SSE events = %v, want %v", got, want)
	}
	var runState runSummary
	if err := json.Unmarshal(startEvents[0].data, &runState); err != nil {
		t.Fatal(err)
	}
	if runState.Status != "running" || !strings.HasPrefix(runState.TurnID, "web-") || runState.StartedAt == "" {
		t.Errorf("run event data = %+v", runState)
	}
	var completedData struct {
		Message webUIMessage `json:"message"`
	}
	if err := json.Unmarshal(startEvents[3].data, &completedData); err != nil {
		t.Fatal(err)
	}
	if completedData.Message.Status != "complete" || completedData.Message.Text != "hello" || len(completedData.Message.Parts) != 1 {
		t.Errorf("completed UI message = %+v", completedData.Message)
	}

	resolver.mu.Lock()
	if len(resolver.calls) != 1 || resolver.calls[0] != [3]string{"session-1", "dev", "models/custom.json"} {
		t.Errorf("resolver calls = %#v", resolver.calls)
	}
	resolver.mu.Unlock()

	attach := httptest.NewRequest(http.MethodGet, "/api/sessions/session-1/run/stream?profile=dev", nil)
	attach.Header.Set("Authorization", "Bearer "+testAPIToken)
	attachResponse := httptest.NewRecorder()
	handler.ServeHTTP(attachResponse, attach)
	if attachResponse.Code != http.StatusOK {
		t.Fatalf("GET run stream status = %d, body=%s", attachResponse.Code, attachResponse.Body.String())
	}
	attachEvents := parseEvents(t, attachResponse.Body.String())
	if got, want := eventNames(attachEvents), []string{"run", "ui.message", "completed", "done"}; !equalStrings(got, want) {
		t.Fatalf("attached SSE events = %v, want %v", got, want)
	}
	var attachedState runSummary
	if err := json.Unmarshal(attachEvents[0].data, &attachedState); err != nil {
		t.Fatal(err)
	}
	if attachedState.Status != "completed" {
		t.Errorf("attached run status = %q, want completed", attachedState.Status)
	}

	stop := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/run/stop?profile=dev", nil)
	stop.Header.Set("Authorization", "Bearer "+testAPIToken)
	stopResponse := httptest.NewRecorder()
	handler.ServeHTTP(stopResponse, stop)
	if stopResponse.Code != http.StatusOK || !strings.Contains(stopResponse.Body.String(), `"stopped":false`) {
		t.Errorf("POST stop response = %d %s", stopResponse.Code, stopResponse.Body.String())
	}
}

func TestRunPublishesReasoningToolProgressAndToolResults(t *testing.T) {
	const completedFirst = `{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"function_call","call_id":"call-1","name":"write","arguments":"{\"path\":\"notes.txt\"}"}]}}`
	const completedFinal = `{"type":"response.completed","response":{"id":"resp-2","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}]}}`
	runner := runnerFunc(func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		send := func(raw string) error {
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(raw), &event); err != nil {
				return err
			}
			return options.OnEvent(responses.Event{Type: event.Type, Raw: json.RawMessage(raw)})
		}
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp-1"}}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"Planning."}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"item-1","type":"function_call","call_id":"call-1","name":"write","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"item-1","output_index":0,"delta":"{\"path\":"}`,
			`{"type":"response.function_call_arguments.done","item_id":"item-1","output_index":0,"arguments":"{\"path\":\"notes.txt\"}"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"item-1","type":"function_call","call_id":"call-1","name":"write","arguments":"{\"path\":\"notes.txt\"}"}}`,
			completedFirst,
		} {
			if err := send(event); err != nil {
				return session.RunResult{}, err
			}
		}
		if options.OnToolResult != nil {
			options.OnToolResult(json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"{\"status\":\"ok\"}\nwritten"}`))
		}
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp-2"}}`,
			`{"type":"response.output_text.delta","delta":"Done."}`,
			completedFinal,
		} {
			if err := send(event); err != nil {
				return session.RunResult{}, err
			}
		}
		return session.RunResult{TurnID: options.TurnID, Event: json.RawMessage(completedFinal)}, nil
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-tools", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken:            testAPIToken,
		MaxBodyBytes:        4096,
		SSEMaxBufferedBytes: 8192,
		RunStateRetention:   time.Minute,
	})
	defer handler.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-tools-progress/run", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("POST run status = %d, body=%s", response.Code, response.Body.String())
	}
	events := parseEvents(t, response.Body.String())
	var sawReasoning, sawToolCall, sawToolResult, sawArgumentDelta bool
	var terminal webUIMessage
	for _, event := range events {
		switch event.name {
		case "ui.message":
			// A part that arrives as the first change of a message is carried by
			// the whole-message snapshot rather than a ui.part event.
			var payload struct {
				Message struct {
					Parts []struct {
						Type string `json:"type"`
					} `json:"parts"`
				} `json:"message"`
			}
			if err := json.Unmarshal(event.data, &payload); err != nil {
				t.Fatal(err)
			}
			for _, part := range payload.Message.Parts {
				sawReasoning = sawReasoning || part.Type == "reasoning"
			}
		case "ui.part":
			var payload struct {
				Part struct {
					Type string `json:"type"`
				} `json:"part"`
			}
			if err := json.Unmarshal(event.data, &payload); err != nil {
				t.Fatal(err)
			}
			sawReasoning = sawReasoning || payload.Part.Type == "reasoning"
			sawToolCall = sawToolCall || payload.Part.Type == "tool_call"
			sawToolResult = sawToolResult || payload.Part.Type == "tool_result"
		case "ui.delta":
			var payload struct {
				Append string `json:"append"`
			}
			if err := json.Unmarshal(event.data, &payload); err != nil {
				t.Fatal(err)
			}
			sawArgumentDelta = sawArgumentDelta || payload.Append == `{"path":`
		case "completed":
			var payload struct {
				Message webUIMessage `json:"message"`
			}
			if err := json.Unmarshal(event.data, &payload); err != nil {
				t.Fatal(err)
			}
			terminal = payload.Message
		}
	}
	if !sawReasoning || !sawToolCall || !sawToolResult || !sawArgumentDelta {
		t.Fatalf("missing live progress events: reasoning=%v tool_call=%v tool_result=%v argument_delta=%v; events=%v", sawReasoning, sawToolCall, sawToolResult, sawArgumentDelta, eventNames(events))
	}
	if terminal.Text != "Done." || terminal.ReasoningContent != "Planning." || len(terminal.Parts) != 4 || terminal.Parts[1].Arguments == nil || *terminal.Parts[1].Arguments != `{"path":"notes.txt"}` || terminal.Parts[2].Status != "success" {
		t.Fatalf("terminal UI message = %+v", terminal)
	}

	attach := httptest.NewRequest(http.MethodGet, "/api/sessions/session-tools-progress/run/stream", nil)
	attach.Header.Set("Authorization", "Bearer "+testAPIToken)
	attached := httptest.NewRecorder()
	handler.ServeHTTP(attached, attach)
	if attached.Code != http.StatusOK {
		t.Fatalf("GET run stream status = %d, body=%s", attached.Code, attached.Body.String())
	}
	attachedEvents := parseEvents(t, attached.Body.String())
	var replayed webUIMessage
	for _, event := range attachedEvents {
		if event.name != "ui.message" {
			continue
		}
		var payload struct {
			Message webUIMessage `json:"message"`
		}
		if err := json.Unmarshal(event.data, &payload); err != nil {
			t.Fatal(err)
		}
		replayed = payload.Message
	}
	if len(replayed.Parts) != 4 || replayed.Parts[0].Type != "reasoning" || replayed.Parts[1].Type != "tool_call" || replayed.Parts[2].Type != "tool_result" || replayed.Parts[3].Text != "Done." {
		t.Fatalf("reattached UI message = %+v", replayed)
	}
}

func TestRunAttachHydratesSpilledTextSnapshot(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"id":"resp_cache","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished"}]}]}}`
	delta := strings.Repeat("x", 8192)
	deltaEvent, err := json.Marshal(struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}{Type: "response.output_text.delta", Delta: delta})
	if err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(t.TempDir(), "responses.partial")
	deltaPublished := make(chan struct{})
	releaseRun := make(chan struct{})
	var releaseOnce sync.Once
	runner := runnerFunc(func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		if options.OnTurnStarted != nil {
			options.OnTurnStarted(partialPath)
		}
		if options.OnEvent != nil {
			if err := options.OnEvent(responses.Event{Type: "response.output_text.delta", Raw: deltaEvent}); err != nil {
				return session.RunResult{}, err
			}
		}
		close(deltaPublished)
		<-releaseRun
		return session.RunResult{TurnID: options.TurnID, Event: json.RawMessage(completed)}, nil
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "cache-session", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken:                  testAPIToken,
		MaxBodyBytes:              4096,
		SSEMaxBufferedBytes:       1 << 20,
		RunStateRetention:         time.Minute,
		RunContentMemoryBytes:     1,
		RunContentWriteBatchBytes: 1024,
		RunContentFlushInterval:   time.Second,
	})
	defer handler.Close()
	defer releaseOnce.Do(func() { close(releaseRun) })

	request := httptest.NewRequest(http.MethodPost, "/api/sessions/cache-session/run", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	requestDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(requestDone)
	}()
	<-deltaPublished

	if content, err := os.ReadFile(partialPath); err != nil || string(content) != delta {
		t.Fatalf("spilled content = %d bytes, error=%v", len(content), err)
	}
	run := handler.runs.current("cache-session\x00")
	if run == nil {
		t.Fatal("active run not found")
	}
	queue := run.subscribe()
	defer run.detach(queue)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if event, ok := queue.next(ctx); !ok || event.name != "run" {
		t.Fatalf("attach run event = %+v, ok=%v", event, ok)
	}
	snapshot, ok := queue.next(ctx)
	if !ok || snapshot.name != "ui.message" {
		t.Fatalf("attach snapshot event = %+v, ok=%v", snapshot, ok)
	}
	parsed := parseEvents(t, string(snapshot.frame))
	var payload struct {
		Message webUIMessage `json:"message"`
	}
	if err := json.Unmarshal(parsed[0].data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Message.Text != delta || len(payload.Message.Parts) != 1 || payload.Message.Parts[0].Text != delta {
		t.Fatalf("hydrated attach snapshot mismatch: text=%d bytes, parts=%+v", len(payload.Message.Text), payload.Message.Parts)
	}

	releaseOnce.Do(func() { close(releaseRun) })
	<-requestDone
	if response.Code != http.StatusOK {
		t.Fatalf("run route status = %d, body=%s", response.Code, response.Body.String())
	}
	handler.runs.retire(run)
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Errorf("retired run partial cache remains: %v", err)
	}
}

func TestRunContinuesAfterSubscriberDisconnectAndCanBeStopped(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return session.RunResult{}, ctx.Err()
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-2", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken:            testAPIToken,
		MaxBodyBytes:        4096,
		SSEMaxBufferedBytes: 4096,
		RunStateRetention:   time.Minute,
	})
	defer handler.Close()

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	start := httptest.NewRequest(http.MethodPost, "/api/sessions/session-2/run", strings.NewReader(`{"input":"hello"}`)).WithContext(requestCtx)
	start.Header.Set("Authorization", "Bearer "+testAPIToken)
	startDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), start)
		close(startDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("session run did not start")
	}
	cancelRequest()
	select {
	case <-startDone:
	case <-time.After(time.Second):
		t.Fatal("disconnected SSE handler did not return")
	}
	select {
	case <-cancelled:
		t.Fatal("disconnect cancelled the accepted session run")
	default:
	}

	stop := httptest.NewRequest(http.MethodPost, "/api/sessions/session-2/run/stop", nil)
	stop.Header.Set("Authorization", "Bearer "+testAPIToken)
	stopResponse := httptest.NewRecorder()
	handler.ServeHTTP(stopResponse, stop)
	if stopResponse.Code != http.StatusOK || !strings.Contains(stopResponse.Body.String(), `"stopped":true`) {
		t.Fatalf("stop response = %d %s", stopResponse.Code, stopResponse.Body.String())
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the accepted session run")
	}
}

func TestRunReattachesByResolvedDefaultProfileAfterSubscriberDisconnect(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return session.RunResult{}, ctx.Err()
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-default", Profile: "default", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken:            testAPIToken,
		MaxBodyBytes:        4096,
		SSEMaxBufferedBytes: 4096,
		RunStateRetention:   time.Minute,
	})
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	start, err := http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL+"/api/sessions/session-default/run?profile=default", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	start.Header.Set("Authorization", "Bearer "+testAPIToken)
	startResponse, err := client.Do(start)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if startResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(startResponse.Body)
		startResponse.Body.Close()
		t.Fatalf("start status = %d, body=%s", startResponse.StatusCode, body)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		startResponse.Body.Close()
		t.Fatal("session run did not start")
	}
	cancelRequest() // Simulate a browser reload closing the original SSE subscriber.
	startResponse.Body.Close()

	attach, err := http.NewRequest(http.MethodGet, server.URL+"/api/sessions/session-default/run/stream?profile=default", nil)
	if err != nil {
		t.Fatal(err)
	}
	attach.Header.Set("Authorization", "Bearer "+testAPIToken)
	attachResponse, err := client.Do(attach)
	if err != nil {
		t.Fatalf("attach to active run: %v", err)
	}
	defer attachResponse.Body.Close()
	if attachResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(attachResponse.Body)
		t.Fatalf("attach status = %d, body=%s", attachResponse.StatusCode, body)
	}

	stop, err := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/session-default/run/stop?profile=default", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop.Header.Set("Authorization", "Bearer "+testAPIToken)
	stopResponse, err := client.Do(stop)
	if err != nil {
		t.Fatalf("stop reattached run: %v", err)
	}
	stopBody, readErr := io.ReadAll(stopResponse.Body)
	stopResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if stopResponse.StatusCode != http.StatusOK || !strings.Contains(string(stopBody), `"stopped":true`) {
		t.Fatalf("stop response = %d %s", stopResponse.StatusCode, stopBody)
	}
	resolver.mu.Lock()
	calls := append([][3]string(nil), resolver.calls...)
	resolver.mu.Unlock()
	wantCalls := [][3]string{
		{"session-default", "default", ""},
		{"session-default", "default", ""},
		{"session-default", "default", ""},
	}
	if len(calls) != len(wantCalls) {
		t.Fatalf("resolver calls = %#v, want %#v", calls, wantCalls)
	}
	for index, call := range calls {
		if call != wantCalls[index] {
			t.Fatalf("resolver call %d = %#v, want %#v", index, call, wantCalls[index])
		}
	}

	attachedBody, err := io.ReadAll(attachResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	attachedEvents := parseEvents(t, string(attachedBody))
	if got, want := eventNames(attachedEvents), []string{"run", "error", "done"}; !equalStrings(got, want) {
		t.Fatalf("reattached events = %v, want %v", got, want)
	}
	var state runSummary
	if err := json.Unmarshal(attachedEvents[0].data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "running" {
		t.Fatalf("reattached run status = %q, want running", state.Status)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the reattached run")
	}
}

func TestHealthAuthCORSAndBodyLimit(t *testing.T) {
	resolver := &fakeResolver{}
	handler := testHandler(t, resolver, Config{
		APIToken:            testAPIToken,
		AllowedOrigins:      []string{"https://ui.example.test"},
		MaxBodyBytes:        32,
		SSEMaxBufferedBytes: 1024,
		RunStateRetention:   time.Minute,
	})
	defer handler.Close()

	health := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	health.Header.Set("Origin", "https://ui.example.test")
	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, health)
	if healthResponse.Code != http.StatusOK || !strings.Contains(healthResponse.Body.String(), `"ok":true`) {
		t.Errorf("health response = %d %s", healthResponse.Code, healthResponse.Body.String())
	}
	if got := healthResponse.Header().Get("Access-Control-Allow-Origin"); got != "https://ui.example.test" {
		t.Errorf("health CORS origin = %q", got)
	}

	unauthorized := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/run/stream", nil)
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized || unauthorizedResponse.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("unauthorized response = %d, headers=%v", unauthorizedResponse.Code, unauthorizedResponse.Header())
	}

	blockedOrigin := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	blockedOrigin.Header.Set("Origin", "https://other.example.test")
	blockedOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(blockedOriginResponse, blockedOrigin)
	if blockedOriginResponse.Code != http.StatusForbidden {
		t.Errorf("blocked origin status = %d", blockedOriginResponse.Code)
	}

	preflight := httptest.NewRequest(http.MethodOptions, "/api/sessions/s1/run", nil)
	preflight.Header.Set("Origin", "https://ui.example.test")
	preflightResponse := httptest.NewRecorder()
	handler.ServeHTTP(preflightResponse, preflight)
	if preflightResponse.Code != http.StatusNoContent || preflightResponse.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("preflight response = %d headers=%v", preflightResponse.Code, preflightResponse.Header())
	}

	catalogPreflight := httptest.NewRequest(http.MethodOptions, "/api/catalog/profiles/dev/coder", nil)
	catalogPreflight.Header.Set("Origin", "https://ui.example.test")
	catalogPreflightResponse := httptest.NewRecorder()
	handler.ServeHTTP(catalogPreflightResponse, catalogPreflight)
	allowedMethods := catalogPreflightResponse.Header().Get("Access-Control-Allow-Methods")
	allowedHeaders := catalogPreflightResponse.Header().Get("Access-Control-Allow-Headers")
	if catalogPreflightResponse.Code != http.StatusNoContent || !strings.Contains(allowedMethods, "PUT") || !strings.Contains(allowedMethods, "DELETE") || !strings.Contains(allowedHeaders, "If-Match") {
		t.Errorf("catalog preflight = %d methods=%q headers=%q", catalogPreflightResponse.Code, allowedMethods, allowedHeaders)
	}

	oversized := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/run", strings.NewReader(`{"input":"this request exceeds the configured body limit"}`))
	oversized.Header.Set("Authorization", "Bearer "+testAPIToken)
	oversizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(oversizedResponse, oversized)
	if oversizedResponse.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized request status = %d, body=%s", oversizedResponse.Code, oversizedResponse.Body.String())
	}
}

func TestNewHandlerRequiresExplicitSafeConfiguration(t *testing.T) {
	resolver := &fakeResolver{}
	for _, config := range []Config{
		{APIToken: "short", MaxBodyBytes: 1024, SSEMaxBufferedBytes: 1024, RunStateRetention: time.Minute, MaxPageTurns: 50},
		{APIToken: testAPIToken, MaxBodyBytes: 0, SSEMaxBufferedBytes: 1024, RunStateRetention: time.Minute, MaxPageTurns: 50},
		{APIToken: testAPIToken, MaxBodyBytes: 1024, SSEMaxBufferedBytes: 1, RunStateRetention: time.Minute, MaxPageTurns: 50},
		{APIToken: testAPIToken, MaxBodyBytes: 1024, SSEMaxBufferedBytes: 1024, MaxPageTurns: 50},
		{APIToken: testAPIToken, MaxBodyBytes: 1024, SSEMaxBufferedBytes: 1024, RunStateRetention: -time.Second, MaxPageTurns: 50},
	} {
		if _, err := NewHandler(config, resolver); err == nil {
			t.Errorf("NewHandler(%+v) succeeded, want validation error", config)
		}
	}
	if _, err := NewHandler(Config{APIToken: testAPIToken, MaxBodyBytes: 1024, SSEMaxBufferedBytes: 1024, RunStateRetention: time.Minute}, nil); err == nil {
		t.Error("NewHandler accepted a nil session resolver")
	}
	zeroRetention, err := NewHandler(Config{
		ShutdownTimeout: 50 * time.Millisecond,
		APIToken:        testAPIToken, MaxBodyBytes: 1024, SSEMaxBufferedBytes: 1024,
		RunStateRetention: 0, AllowZeroRunStateRetention: true, MaxPageTurns: 50,
	}, resolver)
	if err != nil {
		t.Fatalf("NewHandler(explicit zero retention) error = %v", err)
	}
	zeroRetention.Close()
}

func TestNewHandlerValidatesToolStreamInterval(t *testing.T) {
	resolver := &fakeResolver{}
	base := Config{
		APIToken:            testAPIToken,
		MaxBodyBytes:        1024,
		SSEMaxBufferedBytes: 1024,
		RunStateRetention:   time.Minute,
		MaxPageTurns:        50,
	}
	for _, interval := range []time.Duration{-time.Millisecond, time.Minute + time.Millisecond} {
		config := base
		config.ToolStreamInterval = interval
		if _, err := NewHandler(config, resolver); err == nil {
			t.Errorf("NewHandler(ToolStreamInterval=%s) succeeded, want validation error", interval)
		}
	}
	for _, interval := range []time.Duration{0, time.Minute} {
		config := base
		config.ToolStreamInterval = interval
		handler, err := NewHandler(config, resolver)
		if err != nil {
			t.Fatalf("NewHandler(ToolStreamInterval=%s) error = %v", interval, err)
		}
		if handler.toolStreamInterval != interval {
			t.Errorf("handler interval = %s, want %s", handler.toolStreamInterval, interval)
		}
		handler.Close()
	}
}

func TestSessionToolsRouteReturnsEffectiveDefinitionsForProfile(t *testing.T) {
	resolver := &fakeToolLister{
		fakeResolver: &fakeResolver{},
		definitions: []tools.RouteDefinition{{Definition: tools.Definition{
			ID:          "mcp.docs.search",
			Source:      "mcp",
			Server:      "docs",
			Name:        "search",
			Description: "Search documentation",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
		}}, {Definition: tools.Definition{
			ID:          "mcp.docs.fetch",
			Source:      "mcp",
			Server:      "docs",
			Name:        "fetch",
			Description: "Fetch a page",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, Lazy: true}},
	}
	handler := testHandler(t, resolver, Config{
		APIToken:            testAPIToken,
		MaxBodyBytes:        4096,
		SSEMaxBufferedBytes: 4096,
		RunStateRetention:   time.Minute,
	})
	defer handler.Close()

	request := httptest.NewRequest(http.MethodGet, "/api/sessions/session-tools/tools?profile=dev", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET tools status = %d, body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Tools []tools.Definition `json:"tools"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode tools response: %v", err)
	}
	// A lazy tool is a row with lazy true; an eager one has no lazy key at all.
	var rows struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if _, hasLazy := rows.Tools[0]["lazy"]; len(rows.Tools) != 2 || hasLazy || rows.Tools[1]["lazy"] != true || rows.Tools[1]["id"] != "mcp.docs.fetch" {
		t.Fatalf("tools rows = %v, want the eager row without lazy and the lazy row with lazy true", rows.Tools)
	}
	body.Tools = body.Tools[:1]
	if len(body.Tools) != 1 || body.Tools[0].ID != "mcp.docs.search" || body.Tools[0].Server != "docs" || string(body.Tools[0].InputSchema) != `{"type":"object","properties":{"query":{"type":"string"}}}` {
		t.Fatalf("tools response = %+v", body.Tools)
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if len(resolver.calls) != 1 || resolver.calls[0] != [2]string{"session-tools", "dev"} {
		t.Fatalf("ListTools calls = %#v", resolver.calls)
	}
}

type runnerFunc func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error)

func (f runnerFunc) Run(ctx context.Context, input json.RawMessage, options session.RunOptions) (session.RunResult, error) {
	return f(ctx, input, options)
}

type fakeResolver struct {
	mu       sync.Mutex
	resolved ResolvedSession
	calls    [][3]string
	err      error
}

type fakeToolLister struct {
	*fakeResolver
	definitions []tools.RouteDefinition
	hidden      []tools.Definition
	calls       [][2]string
}

func (r *fakeToolLister) ListTools(_ context.Context, reference, profile string) (SessionTools, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [2]string{reference, profile})
	return SessionTools{Tools: append([]tools.RouteDefinition(nil), r.definitions...), Hidden: r.hidden}, r.err
}

func (r *fakeResolver) Resolve(_ context.Context, reference, profile, model string) (ResolvedSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [3]string{reference, profile, model})
	if r.err != nil {
		return ResolvedSession{}, r.err
	}
	return r.resolved, nil
}

func testHandler(t *testing.T, resolver SessionResolver, config Config) *Handler {
	t.Helper()
	if config.ShutdownTimeout == 0 {
		config.ShutdownTimeout = 50 * time.Millisecond
	}
	if config.SnapshotTurns == 0 {
		config.SnapshotTurns = 24
	}
	if config.MaxPageTurns == 0 {
		config.MaxPageTurns = 50
	}
	if config.MaxInlinePartBytes == 0 {
		config.MaxInlinePartBytes = 16 * 1024
	}
	handler, err := NewHandler(config, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if fake, ok := resolver.(interface {
		maintenanceFake() *fakeMaintenanceResolver
	}); ok && fake.maintenanceFake().activeRun == nil {
		fake.maintenanceFake().activeRun = handler.RunActive
	}
	return handler
}

type parsedEvent struct {
	name string
	data json.RawMessage
}

func parseEvents(t *testing.T, body string) []parsedEvent {
	t.Helper()
	blocks := strings.Split(strings.TrimSpace(body), "\n\n")
	events := make([]parsedEvent, 0, len(blocks))
	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var event parsedEvent
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				event.name = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				event.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			}
		}
		if event.name == "" || !json.Valid(event.data) {
			t.Fatalf("invalid SSE block %q", block)
		}
		events = append(events, event)
	}
	return events
}

func eventNames(events []parsedEvent) []string {
	names := make([]string, len(events))
	for index, event := range events {
		names[index] = event.name
	}
	return names
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
