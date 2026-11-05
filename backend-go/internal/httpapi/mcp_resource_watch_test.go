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

type fakeMCPResourceWatch struct {
	*fakeMCPResources
	subscribed []string
	after      []int64
}

func (r *fakeMCPResourceWatch) SubscribeMCPResource(_ context.Context, _, _, server, uri string, on bool) error {
	if server == "missing" {
		return &StatusError{Code: http.StatusNotFound, Message: "no MCP server"}
	}
	state := "off"
	if on {
		state = "on"
	}
	r.subscribed = append(r.subscribed, server+" "+uri+" "+state)
	return nil
}

func (r *fakeMCPResourceWatch) MCPResourceEvents(_ context.Context, _, _ string, after int64) (MCPResourceEvents, error) {
	r.after = append(r.after, after)
	return MCPResourceEvents{Latest: 7, Events: []MCPResourceEvent{{Seq: 7, Server: "docs", Kind: "updated", URI: "docs://plan", At: time.Unix(0, 0).UTC()}}}, nil
}

func TestMCPResourceWatchRoutes(t *testing.T) {
	resolver := &fakeMCPResourceWatch{fakeMCPResources: &fakeMCPResources{fakeResolver: &fakeResolver{}}}
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
	if response := send(http.MethodPost, "/api/sessions/demo/mcp-resources/subscribe", `{"server":"docs","uri":"docs://plan"}`); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"subscribed":true`) {
		t.Fatalf("subscribe: %d %s", response.Code, response.Body)
	}
	if response := send(http.MethodPost, "/api/sessions/demo/mcp-resources/unsubscribe", `{"server":"docs","uri":"docs://plan"}`); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"subscribed":false`) {
		t.Fatalf("unsubscribe: %d %s", response.Code, response.Body)
	}
	if strings.Join(resolver.subscribed, "|") != "docs docs://plan on|docs docs://plan off" {
		t.Fatalf("subscribed = %v", resolver.subscribed)
	}
	for body, want := range map[string]string{
		`{"server":"docs"}`:                        "uri must be a non-empty string",
		`{"uri":"x"}`:                              "server must be a non-empty string",
		`{"server":"docs","uri":"x","extra":1}`:    "unknown field extra",
		`{"server":"missing","uri":"docs://plan"}`: "no MCP server",
	} {
		if response := send(http.MethodPost, "/api/sessions/demo/mcp-resources/subscribe", body); response.Code < 400 || !strings.Contains(response.Body.String(), want) {
			t.Errorf("%s: %d %s", body, response.Code, response.Body)
		}
	}

	response := send(http.MethodGet, "/api/sessions/demo/mcp-resources/events?after=3", "")
	var events MCPResourceEvents
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &events) != nil || events.Latest != 7 || len(events.Events) != 1 || events.Subscriptions == nil {
		t.Fatalf("events: %d %s", response.Code, response.Body)
	}
	if len(resolver.after) != 1 || resolver.after[0] != 3 {
		t.Fatalf("after = %v", resolver.after)
	}
	if response := send(http.MethodGet, "/api/sessions/demo/mcp-resources/events?after=-1", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("negative after: %d", response.Code)
	}
}

// Without the watch port the routes answer 501.
func TestMCPResourceWatchRoutesWithoutThePort(t *testing.T) {
	resolver := &fakeMCPResources{fakeResolver: &fakeResolver{}}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/demo/mcp-resources/events", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("events without the port: %d %s", response.Code, response.Body)
	}
}
