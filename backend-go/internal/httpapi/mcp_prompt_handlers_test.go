package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeMCPPrompts struct {
	*fakeResolver
	listCalls [][2]string
	requests  []MCPPromptRequest
	getErr    error
}

func (r *fakeMCPPrompts) ListMCPPrompts(_ context.Context, reference, profile string) (MCPPromptList, error) {
	r.listCalls = append(r.listCalls, [2]string{reference, profile})
	return MCPPromptList{
		Prompts:  []MCPPrompt{{Server: "gh", Name: "review", Arguments: []MCPPromptArgument{{Name: "pr", Required: true}}}},
		Failures: []MCPPromptFailure{{Server: "down", Error: "failed to connect"}},
	}, nil
}

func (r *fakeMCPPrompts) GetMCPPrompt(_ context.Context, _, _ string, request MCPPromptRequest) (MCPPromptText, error) {
	r.requests = append(r.requests, request)
	if r.getErr != nil {
		return MCPPromptText{}, r.getErr
	}
	return MCPPromptText{Server: request.Server, Name: request.Name, Text: "Review PR " + request.Arguments["pr"]}, nil
}

func TestMCPPromptRoutes(t *testing.T) {
	resolver := &fakeMCPPrompts{fakeResolver: &fakeResolver{}}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()
	send := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testAPIToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	response := send(http.MethodGet, "/api/sessions/demo/mcp-prompts?profile=dev%2Fcoder", "")
	if response.Code != http.StatusOK || len(resolver.listCalls) != 1 || resolver.listCalls[0] != [2]string{"demo", "dev/coder"} {
		t.Fatalf("list: %d %s %v", response.Code, response.Body, resolver.listCalls)
	}
	var listed MCPPromptList
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || len(listed.Prompts) != 1 || listed.Prompts[0].Arguments[0].Name != "pr" || listed.Failures[0].Server != "down" {
		t.Fatalf("list body: %s %v", response.Body, err)
	}

	response = send(http.MethodPost, "/api/sessions/demo/mcp-prompts/get", `{"server":"gh","name":"review","arguments":{"pr":"42"}}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"text":"Review PR 42"`) {
		t.Fatalf("get: %d %s", response.Code, response.Body)
	}

	for body, want := range map[string]string{
		`{"name":"review"}`: "server must be a non-empty string",
		`{"server":"gh"}`:   "name must be a non-empty string",
		`{"server":"gh","name":"r","arguments":{"a":1}}`: "arguments must be an object of strings",
		`{"server":"gh","name":"r","extra":true}`:        "unknown fields: extra",
		`[]`: "body must be a JSON object",
	} {
		response := send(http.MethodPost, "/api/sessions/demo/mcp-prompts/get", body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), want) {
			t.Errorf("%s: %d %s, want 400 with %q", body, response.Code, response.Body, want)
		}
	}

	resolver.getErr = &StatusError{Code: http.StatusBadGateway, Message: "MCP server gh: prompt review: the server refused: no such PR"}
	response = send(http.MethodPost, "/api/sessions/demo/mcp-prompts/get", `{"server":"gh","name":"review"}`)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "no such PR") {
		t.Fatalf("refused: %d %s", response.Code, response.Body)
	}
	if response := send(http.MethodGet, "/api/sessions/demo/mcp-prompts/get", ""); response.Code != http.StatusNotFound {
		t.Fatalf("GET of the get route: %d", response.Code)
	}

	// A resolver without MCP prompts answers 501.
	plain := testHandler(t, &fakeResolver{}, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer plain.Close()
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/demo/mcp-prompts", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	recorder := httptest.NewRecorder()
	plain.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("no provider: %d", recorder.Code)
	}
}
