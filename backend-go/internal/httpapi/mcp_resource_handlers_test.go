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

type fakeMCPResources struct {
	*fakeResolver
	listCalls [][2]string
	requests  []MCPResourceRequest
	readErr   error
}

func (r *fakeMCPResources) ListMCPResources(_ context.Context, reference, profile string) (MCPResourceList, error) {
	r.listCalls = append(r.listCalls, [2]string{reference, profile})
	return MCPResourceList{
		Resources: []MCPResource{
			{Server: "docs", URI: "docs://guide", Name: "guide", MIMEType: "text/markdown"},
			{Server: "docs", URITemplate: "docs://pages/{name}", Variables: []string{"name"}, Name: "page"},
		},
		Failures: []MCPPromptFailure{{Server: "down", Error: "failed to connect"}},
	}, nil
}

func (r *fakeMCPResources) ReadMCPResource(_ context.Context, _, _ string, request MCPResourceRequest) (MCPResourceText, error) {
	r.requests = append(r.requests, request)
	if r.readErr != nil {
		return MCPResourceText{}, r.readErr
	}
	return MCPResourceText{Server: request.Server, URI: request.URI, Text: "text of " + request.URI + request.Variables["name"]}, nil
}

func TestMCPResourceRoutes(t *testing.T) {
	resolver := &fakeMCPResources{fakeResolver: &fakeResolver{}}
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

	response := send(http.MethodGet, "/api/sessions/demo/mcp-resources?profile=dev%2Fcoder", "")
	if response.Code != http.StatusOK || len(resolver.listCalls) != 1 || resolver.listCalls[0] != [2]string{"demo", "dev/coder"} {
		t.Fatalf("list: %d %s %v", response.Code, response.Body, resolver.listCalls)
	}
	var listed MCPResourceList
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || len(listed.Resources) != 2 || listed.Resources[1].Variables[0] != "name" || listed.Failures[0].Server != "down" {
		t.Fatalf("list body: %s %v", response.Body, err)
	}
	if strings.Contains(response.Body.String(), `"uri":""`) || strings.Contains(response.Body.String(), `"uri_template":""`) {
		t.Fatalf("empty fields were sent: %s", response.Body)
	}

	response = send(http.MethodPost, "/api/sessions/demo/mcp-resources/read", `{"server":"docs","uri":"docs://guide"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"text":"text of docs://guide"`) {
		t.Fatalf("read: %d %s", response.Code, response.Body)
	}
	response = send(http.MethodPost, "/api/sessions/demo/mcp-resources/read", `{"server":"docs","uri_template":"docs://pages/{name}","variables":{"name":"intro"}}`)
	if response.Code != http.StatusOK || resolver.requests[1].URITemplate != "docs://pages/{name}" || resolver.requests[1].Variables["name"] != "intro" {
		t.Fatalf("read template: %d %s %+v", response.Code, response.Body, resolver.requests)
	}

	for body, want := range map[string]string{
		`{"uri":"x"}`:       "server must be a non-empty string",
		`{"server":"docs"}`: "give uri or uri_template, not both",
		`{"server":"docs","uri":"a","uri_template":"b"}`:           "give uri or uri_template, not both",
		`{"server":"docs","uri":""}`:                               "uri must be a non-empty string",
		`{"server":"docs","uri":"a","variables":{"x":"y"}}`:        "variables go with uri_template only",
		`{"server":"docs","uri_template":"b","variables":{"x":1}}`: "variables must be an object of strings",
		`{"server":"docs","uri":"a","extra":true}`:                 "unknown fields: extra",
	} {
		response := send(http.MethodPost, "/api/sessions/demo/mcp-resources/read", body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), want) {
			t.Errorf("%s: %d %s, want 400 with %q", body, response.Code, response.Body, want)
		}
	}

	resolver.readErr = &StatusError{Code: http.StatusBadGateway, Message: "MCP server docs: resource docs://logo has no text to insert"}
	response = send(http.MethodPost, "/api/sessions/demo/mcp-resources/read", `{"server":"docs","uri":"docs://logo"}`)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "no text to insert") {
		t.Fatalf("refused: %d %s", response.Code, response.Body)
	}

	plain := testHandler(t, &fakeResolver{}, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer plain.Close()
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/demo/mcp-resources", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	recorder := httptest.NewRecorder()
	plain.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("no provider: %d", recorder.Code)
	}
}
