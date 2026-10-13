package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

const integrationAPIToken = "0123456789abcdef0123456789abcdef"

func TestMultiRootHTTPRunUsesQualifiedSessionAndGoldenTranscript(t *testing.T) {
	const outputItem = `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`
	const delta1 = `{"type":"response.output_text.delta","delta":"hel"}`
	const delta2 = `{"type":"response.output_text.delta","delta":"lo"}`
	const completed = `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9,"provider_extra":"discarded"},"output":[],"future_response_field":"retained"}}`
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer local-provider-key" {
			http.Error(w, "unexpected provider credentials", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, payload := range []string{delta1, delta2, outputItem, completed, "[DONE]"} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
		}
	}))
	defer provider.Close()

	sharedSettings := map[string]any{
		"http_debug":  map[string]any{"enabled": true, "include_request_headers": true, "include_request_body": true, "include_response_body": true},
		"persistence": map[string]any{"durability": "process", "max_item_bytes": 1048576, "response_metadata": "full"},
	}
	config, fixture := layoutConfig(t, layouttest.Spec{
		SessionID: "shared", RootID: "alpha", ExtraRoots: []string{"beta"},
		Server:   map[string]any{"max_body_bytes": 4096, "sse_max_buffered_bytes": 4096, "max_inline_part_bytes": 3},
		Settings: sharedSettings,
		Models:   map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	fixture.AddSessionInRoot(t, "beta", "second", fixture.CWD, "dev/coder", sharedSettings)
	alphaLocation, err := sessionfs.Resolve(config.Roots, "alpha:shared")
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := sessionfs.LoadSessionSettings(alphaLocation, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(alpha.MessagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	backupLine := `{"type":"message","role":"user","content":[{"type":"input_text","text":"archived"}]}`
	if err := os.WriteFile(alpha.MessagesBackupPath, []byte(backupLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() {
		service.Close()
		api.Close()
	}()

	response := postRun(t, api.URL+"/api/sessions/alpha:shared/run", integrationAPIToken)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("qualified run status = %d, body=%s", response.StatusCode, body)
	}
	responseText, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"event: run", "event: ui.message", "event: ui.delta", "event: usage", "event: completed", "event: done"} {
		if !strings.Contains(string(responseText), event) {
			t.Errorf("qualified run stream missing %q: %s", event, responseText)
		}
	}
	debugRequest := readDebugRecordEventually(t, filepath.Join(alpha.StoragePaths.HTTPDebug, "request1"))
	requestHeaders, ok := debugRequest["headers"].(map[string]any)
	if !ok || debugRequest["direction"] != "request" || requestHeaders["authorization"] != "[REDACTED]" {
		t.Fatalf("HTTP debug request record = %#v", debugRequest)
	}
	debugResponse := readDebugRecordEventually(t, filepath.Join(alpha.StoragePaths.HTTPDebug, "response1"))
	responseHeaders, ok := debugResponse["headers"].(map[string]any)
	if !ok || debugResponse["direction"] != "response" || debugResponse["status"] != float64(http.StatusOK) || responseHeaders["content-type"] != "text/event-stream" {
		t.Fatalf("HTTP debug response record = %#v", debugResponse)
	}
	rawResponse, err := os.ReadFile(filepath.Join(alpha.StoragePaths.HTTPDebug, "response1.body"))
	if err != nil || !strings.HasSuffix(string(rawResponse), "data: [DONE]\n\n") || !strings.Contains(string(rawResponse), "data: "+delta1+"\n\n") {
		t.Fatalf("raw provider stream was not saved as received: %q, %v", rawResponse, err)
	}
	rawRequest, err := os.ReadFile(filepath.Join(alpha.StoragePaths.HTTPDebug, "request1.body"))
	if err != nil || !json.Valid(rawRequest) || debugRequest["body_bytes"] != float64(len(rawRequest)) {
		t.Fatalf("raw request body = %q, %v, record %#v", rawRequest, err, debugRequest)
	}
	for _, value := range []string{`"input_tokens":7`, `"output_tokens":2`, `"total_tokens":9`, `"completed_responses":1`, `"turn_count":1`, `"history_partial":true`} {
		if !strings.Contains(string(responseText), value) {
			t.Errorf("qualified run usage missing %q: %s", value, responseText)
		}
	}

	attached := getAPI(t, api.URL, "/api/sessions/alpha:shared/run/stream", integrationAPIToken, "")
	attachedText, err := io.ReadAll(attached.Body)
	attached.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if attached.StatusCode != http.StatusOK {
		t.Fatalf("run attach status = %d, body=%s", attached.StatusCode, attachedText)
	}
	for _, event := range []string{"event: run", "event: usage", "event: ui.message", "event: completed", "event: done"} {
		if !strings.Contains(string(attachedText), event) {
			t.Errorf("attached run stream missing %q: %s", event, attachedText)
		}
	}

	transcript, err := os.Open(alpha.MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	var lines []string
	scanner := bufio.NewScanner(transcript)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("messages.jsonl has %d records, want 2", len(lines))
	}
	golden, err := os.ReadFile(filepath.Join("..", "session", "testdata", "messages-session-response.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := lines[1], strings.TrimSuffix(string(golden), "\n"); got != want {
		t.Fatalf("qualified run response differs from Node golden\n got: %s\nwant: %s", got, want)
	}

	listResponse := getAPI(t, api.URL, "/api/sessions", integrationAPIToken, "")
	if listResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResponse.Body)
		listResponse.Body.Close()
		t.Fatalf("session list status = %d, body=%s", listResponse.StatusCode, body)
	}
	type runState struct {
		TurnID string `json:"turn_id"`
		Status string `json:"status"`
	}
	var list struct {
		Sessions []struct {
			ID            string              `json:"id"`
			RootID        string              `json:"root_id"`
			SessionName   string              `json:"session_name"`
			Turns         int64               `json:"turns"`
			Run           *runState           `json:"run,omitempty"`
			RunsByProfile map[string]runState `json:"runs_by_profile,omitempty"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(listResponse.Body).Decode(&list); err != nil {
		listResponse.Body.Close()
		t.Fatal(err)
	}
	listETag := listResponse.Header.Get("ETag")
	listResponse.Body.Close()
	if len(list.Sessions) != 2 || list.Sessions[0].ID != "alpha:shared" || list.Sessions[0].RootID != "alpha" || list.Sessions[0].SessionName != "shared" || list.Sessions[0].Turns != 1 || list.Sessions[1].ID != "beta:second" || list.Sessions[1].Turns != 0 || list.Sessions[1].RootID != "beta" {
		t.Fatalf("session list = %+v", list.Sessions)
	}
	primaryRun := list.Sessions[0].Run
	if primaryRun == nil || primaryRun.Status != "completed" || !strings.HasPrefix(primaryRun.TurnID, "web-") {
		t.Fatalf("session-list run summary = %+v", primaryRun)
	}
	// A layout session always has a named profile, so its run is also listed under it.
	if byProfile := list.Sessions[0].RunsByProfile["dev/coder"]; len(list.Sessions[0].RunsByProfile) != 1 || byProfile.Status != "completed" || byProfile.TurnID != primaryRun.TurnID || list.Sessions[1].Run != nil {
		t.Fatalf("session-list profile run summaries = %+v", list.Sessions)
	}
	if listETag == "" {
		t.Fatal("session list omitted ETag")
	}
	listNotModified := getAPI(t, api.URL, "/api/sessions", integrationAPIToken, listETag)
	listNotModified.Body.Close()
	if listNotModified.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional session list status = %d", listNotModified.StatusCode)
	}

	type truncation struct {
		Bytes int `json:"bytes"`
	}
	type part struct {
		Type      string      `json:"type"`
		Text      string      `json:"text"`
		Truncated *truncation `json:"truncated"`
	}
	type message struct {
		ID     string `json:"id"`
		Sender string `json:"sender"`
		Text   string `json:"text"`
		Cursor *int64 `json:"cursor"`
		Parts  []part `json:"parts"`
	}
	type page struct {
		HasMore     bool   `json:"has_more"`
		NextBefore  *int64 `json:"next_before"`
		Generation  string `json:"generation"`
		StaleCursor bool   `json:"stale_cursor"`
	}
	type historyRef struct {
		ETag  string `json:"etag"`
		Count int    `json:"count"`
	}
	type snapshot struct {
		ID            string              `json:"id"`
		Turns         int64               `json:"turns"`
		Messages      []message           `json:"messages"`
		Page          page                `json:"page"`
		History       *historyRef         `json:"history"`
		Run           *runState           `json:"run,omitempty"`
		RunsByProfile map[string]runState `json:"runs_by_profile,omitempty"`
	}
	firstPageResponse := getAPI(t, api.URL, "/api/sessions/alpha:shared?turns=1", integrationAPIToken, "")
	if firstPageResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(firstPageResponse.Body)
		firstPageResponse.Body.Close()
		t.Fatalf("session snapshot status = %d, body=%s", firstPageResponse.StatusCode, body)
	}
	var firstPage snapshot
	if err := json.NewDecoder(firstPageResponse.Body).Decode(&firstPage); err != nil {
		firstPageResponse.Body.Close()
		t.Fatal(err)
	}
	snapshotETag := firstPageResponse.Header.Get("ETag")
	firstPageResponse.Body.Close()
	if firstPage.ID != "alpha:shared" || firstPage.Turns != 1 || len(firstPage.Messages) != 2 || firstPage.Page.HasMore || firstPage.Page.Generation == "" {
		t.Fatalf("first session snapshot = %+v", firstPage)
	}
	if firstPage.Run == nil || firstPage.Run.TurnID != primaryRun.TurnID || firstPage.Run.Status != "completed" {
		t.Fatalf("snapshot run summary = %+v", firstPage.Run)
	}
	if byProfile := firstPage.RunsByProfile["dev/coder"]; len(firstPage.RunsByProfile) != 1 || byProfile.TurnID != primaryRun.TurnID {
		t.Fatalf("snapshot runs_by_profile = %+v, want only dev/coder", firstPage.RunsByProfile)
	}
	if firstPage.Messages[0].Sender != "user" || firstPage.Messages[0].Text != "hel" || firstPage.Messages[1].Sender != "assistant" || firstPage.Messages[1].Text != "hel" {
		t.Errorf("first page messages = %+v", firstPage.Messages)
	}
	for _, cappedMessage := range firstPage.Messages {
		if cappedMessage.Cursor == nil || len(cappedMessage.Parts) != 1 || cappedMessage.Parts[0].Text != "hel" || cappedMessage.Parts[0].Truncated == nil || cappedMessage.Parts[0].Truncated.Bytes != 5 {
			t.Errorf("capped snapshot message metadata = %+v", cappedMessage)
		}
	}
	if snapshotETag == "" {
		t.Fatal("session snapshot omitted ETag")
	}
	if firstPage.History == nil || firstPage.History.ETag == "" || firstPage.History.Count != 1 {
		t.Fatalf("snapshot history reference = %+v", firstPage.History)
	}
	historyResponse := getAPI(t, api.URL, "/api/sessions/alpha:shared/history", integrationAPIToken, "")
	if historyResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(historyResponse.Body)
		historyResponse.Body.Close()
		t.Fatalf("history status = %d, body=%s", historyResponse.StatusCode, body)
	}
	var history struct {
		Messages []message `json:"messages"`
	}
	if err := json.NewDecoder(historyResponse.Body).Decode(&history); err != nil {
		historyResponse.Body.Close()
		t.Fatal(err)
	}
	historyETag := historyResponse.Header.Get("ETag")
	historyResponse.Body.Close()
	if len(history.Messages) != 1 || history.Messages[0].Sender != "user" || history.Messages[0].Text != "arc" || historyETag == "" {
		t.Fatalf("history = %+v etag=%q", history.Messages, historyETag)
	}
	if len(history.Messages[0].Parts) != 1 || history.Messages[0].Parts[0].Text != "arc" || history.Messages[0].Parts[0].Truncated == nil || history.Messages[0].Parts[0].Truncated.Bytes != len("archived") {
		t.Fatalf("history truncation metadata = %+v", history.Messages[0])
	}
	historyFullResponse := getAPI(t, api.URL, fmt.Sprintf("/api/sessions/alpha:shared/messages/%s", history.Messages[0].ID), integrationAPIToken, "")
	if historyFullResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(historyFullResponse.Body)
		historyFullResponse.Body.Close()
		t.Fatalf("full history message status = %d, body=%s", historyFullResponse.StatusCode, body)
	}
	var fullHistoryMessage struct {
		Message message `json:"message"`
	}
	if err := json.NewDecoder(historyFullResponse.Body).Decode(&fullHistoryMessage); err != nil {
		historyFullResponse.Body.Close()
		t.Fatal(err)
	}
	historyFullResponse.Body.Close()
	if fullHistoryMessage.Message.Text != "archived" || len(fullHistoryMessage.Message.Parts) != 1 || fullHistoryMessage.Message.Parts[0].Truncated != nil {
		t.Fatalf("full history message = %+v", fullHistoryMessage.Message)
	}
	historyNotModified := getAPI(t, api.URL, "/api/sessions/alpha:shared/history", integrationAPIToken, historyETag)
	historyNotModified.Body.Close()
	if historyNotModified.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional history status = %d", historyNotModified.StatusCode)
	}

	for _, cappedMessage := range firstPage.Messages {
		if cappedMessage.ID == "" || cappedMessage.Cursor == nil {
			t.Fatalf("capped message lacks retrieval identity: %+v", cappedMessage)
		}
		messagePath := fmt.Sprintf("/api/sessions/alpha:shared/messages/%s?cursor=%d", cappedMessage.ID, *cappedMessage.Cursor)
		fullMessageResponse := getAPI(t, api.URL, messagePath, integrationAPIToken, "")
		if fullMessageResponse.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(fullMessageResponse.Body)
			fullMessageResponse.Body.Close()
			t.Fatalf("full message status = %d, body=%s", fullMessageResponse.StatusCode, body)
		}
		var fullMessage struct {
			Message message `json:"message"`
		}
		if err := json.NewDecoder(fullMessageResponse.Body).Decode(&fullMessage); err != nil {
			fullMessageResponse.Body.Close()
			t.Fatal(err)
		}
		fullMessageResponse.Body.Close()
		if fullMessage.Message.Sender != cappedMessage.Sender || fullMessage.Message.Text != "hello" || len(fullMessage.Message.Parts) != 1 || fullMessage.Message.Parts[0].Text != "hello" || fullMessage.Message.Parts[0].Truncated != nil {
			t.Fatalf("full message = %+v", fullMessage.Message)
		}
	}

	snapshotNotModified := getAPI(t, api.URL, "/api/sessions/alpha:shared?turns=1", integrationAPIToken, snapshotETag)
	snapshotNotModified.Body.Close()
	if snapshotNotModified.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional session snapshot status = %d", snapshotNotModified.StatusCode)
	}

	secondRun := postRun(t, api.URL+"/api/sessions/alpha:shared/run", integrationAPIToken)
	secondRunBody, err := io.ReadAll(secondRun.Body)
	secondRun.Body.Close()
	if err != nil || secondRun.StatusCode != http.StatusOK {
		t.Fatalf("second run failed: status=%d error=%v body=%s", secondRun.StatusCode, err, secondRunBody)
	}
	lastPageResponse := getAPI(t, api.URL, "/api/sessions/alpha:shared?turns=1", integrationAPIToken, "")
	if lastPageResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(lastPageResponse.Body)
		lastPageResponse.Body.Close()
		t.Fatalf("last page status = %d, body=%s", lastPageResponse.StatusCode, body)
	}
	var lastPage snapshot
	if err := json.NewDecoder(lastPageResponse.Body).Decode(&lastPage); err != nil {
		lastPageResponse.Body.Close()
		t.Fatal(err)
	}
	lastPageResponse.Body.Close()
	if lastPage.Turns != 2 || len(lastPage.Messages) != 2 || !lastPage.Page.HasMore || lastPage.Page.NextBefore == nil {
		t.Fatalf("last page = %+v", lastPage)
	}
	olderPath := fmt.Sprintf("/api/sessions/alpha:shared?before=%d&cursor_generation=%s&turns=1", *lastPage.Page.NextBefore, lastPage.Page.Generation)
	olderResponse := getAPI(t, api.URL, olderPath, integrationAPIToken, "")
	if olderResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(olderResponse.Body)
		olderResponse.Body.Close()
		t.Fatalf("older page status = %d, body=%s", olderResponse.StatusCode, body)
	}
	var olderPage snapshot
	if err := json.NewDecoder(olderResponse.Body).Decode(&olderPage); err != nil {
		olderResponse.Body.Close()
		t.Fatal(err)
	}
	olderResponse.Body.Close()
	if olderPage.Turns != 2 || len(olderPage.Messages) != 2 || olderPage.Page.HasMore || olderPage.Messages[0].Sender != "user" {
		t.Fatalf("older page = %+v", olderPage)
	}
	stalePath := fmt.Sprintf("/api/sessions/alpha:shared?before=%d&cursor_generation=999999:999999:999999&turns=1", *lastPage.Page.NextBefore)
	staleResponse := getAPI(t, api.URL, stalePath, integrationAPIToken, "")
	defer staleResponse.Body.Close()
	var stalePage snapshot
	if err := json.NewDecoder(staleResponse.Body).Decode(&stalePage); err != nil {
		t.Fatal(err)
	}
	if !stalePage.Page.StaleCursor || len(stalePage.Messages) != 0 {
		t.Fatalf("stale cursor response = %+v", stalePage)
	}
}

func getAPI(t *testing.T, address, path, token, etag string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, address+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func postRun(t *testing.T, address, token string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, address, strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readDebugRecordEventually(t *testing.T, path string) map[string]any {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			var record map[string]any
			if err := json.Unmarshal(contents, &record); err != nil {
				t.Fatalf("decode HTTP debug record %s: %v", path, err)
			}
			return record
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read HTTP debug record %s: %v", path, err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for HTTP debug record %s", path)
		case <-ticker.C:
		}
	}
}
