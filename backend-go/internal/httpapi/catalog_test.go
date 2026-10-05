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

func TestSessionCatalogRoutesAndConditionalResponses(t *testing.T) {
	resolver := &fakeCatalogResolver{
		fakeResolver: &fakeResolver{},
		list:         CatalogResponse{Body: json.RawMessage(`{"sessions":[]}`), ETag: `"list-v1"`},
		snapshot:     CatalogResponse{Body: json.RawMessage(`{"id":"root-a:session-1"}`), ETag: `"snapshot-v1"`},
		history:      CatalogResponse{Body: json.RawMessage(`{"messages":[]}`), ETag: `"history-v1"`},
		message:      json.RawMessage(`{"id":"message-1"}`),
	}
	handler := testHandler(t, resolver, Config{
		APIToken:            testAPIToken,
		MaxBodyBytes:        4096,
		SSEMaxBufferedBytes: 4096,
		RunStateRetention:   time.Minute,
		ConditionalRequests: true,
	})
	defer handler.Close()

	list := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	list.Header.Set("Authorization", "Bearer "+testAPIToken)
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK || listResponse.Body.String() != `{"sessions":[]}` {
		t.Fatalf("session list response = %d %s", listResponse.Code, listResponse.Body.String())
	}
	if got := listResponse.Header().Get("ETag"); got != `"list-v1"` {
		t.Errorf("session list ETag = %q", got)
	}

	notModified := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	notModified.Header.Set("Authorization", "Bearer "+testAPIToken)
	notModified.Header.Set("If-None-Match", `W/"list-v1"`)
	notModifiedResponse := httptest.NewRecorder()
	handler.ServeHTTP(notModifiedResponse, notModified)
	if notModifiedResponse.Code != http.StatusNotModified || notModifiedResponse.Body.Len() != 0 {
		t.Fatalf("conditional list response = %d %s", notModifiedResponse.Code, notModifiedResponse.Body.String())
	}

	snapshot := httptest.NewRequest(http.MethodGet, "/api/sessions/root-a%3Asession-1?profile=dev&before=42&cursor_generation=1:2:3&turns=1", nil)
	snapshot.Header.Set("Authorization", "Bearer "+testAPIToken)
	snapshotResponse := httptest.NewRecorder()
	handler.ServeHTTP(snapshotResponse, snapshot)
	if snapshotResponse.Code != http.StatusOK || snapshotResponse.Body.String() != `{"id":"root-a:session-1"}` {
		t.Fatalf("session snapshot response = %d %s", snapshotResponse.Code, snapshotResponse.Body.String())
	}
	if resolver.reference != "root-a:session-1" || resolver.profile != "dev" {
		t.Errorf("snapshot selection = %q/%q", resolver.reference, resolver.profile)
	}
	if resolver.options.Before == nil || *resolver.options.Before != 42 || resolver.options.CursorGeneration != "1:2:3" || resolver.options.Turns == nil || *resolver.options.Turns != 1 {
		t.Errorf("snapshot pagination options = %+v", resolver.options)
	}

	snapshotNotModified := httptest.NewRequest(http.MethodGet, "/api/sessions/root-a:session-1", nil)
	snapshotNotModified.Header.Set("Authorization", "Bearer "+testAPIToken)
	snapshotNotModified.Header.Set("If-None-Match", `W/"snapshot-v1"`)
	snapshotNotModifiedResponse := httptest.NewRecorder()
	handler.ServeHTTP(snapshotNotModifiedResponse, snapshotNotModified)
	if snapshotNotModifiedResponse.Code != http.StatusNotModified || snapshotNotModifiedResponse.Body.Len() != 0 {
		t.Fatalf("conditional snapshot response = %d %s", snapshotNotModifiedResponse.Code, snapshotNotModifiedResponse.Body.String())
	}

	history := httptest.NewRequest(http.MethodGet, "/api/sessions/root-a%3Asession-1/history?profile=dev", nil)
	history.Header.Set("Authorization", "Bearer "+testAPIToken)
	history.Header.Set("If-None-Match", `W/"history-v1"`)
	historyResponse := httptest.NewRecorder()
	handler.ServeHTTP(historyResponse, history)
	if historyResponse.Code != http.StatusNotModified || historyResponse.Body.Len() != 0 {
		t.Fatalf("conditional history response = %d %s", historyResponse.Code, historyResponse.Body.String())
	}

	cursorValue := int64(7)
	resolver.message = json.RawMessage(`{"id":"message-1"}`)
	messageRequest := httptest.NewRequest(http.MethodGet, "/api/sessions/root-a%3Asession-1/messages/message-1?profile=dev&cursor=7", nil)
	messageRequest.Header.Set("Authorization", "Bearer "+testAPIToken)
	messageResponse := httptest.NewRecorder()
	handler.ServeHTTP(messageResponse, messageRequest)
	if messageResponse.Code != http.StatusOK {
		t.Fatalf("message response = %d %s", messageResponse.Code, messageResponse.Body.String())
	}
	var messageBody struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err := json.NewDecoder(messageResponse.Body).Decode(&messageBody); err != nil || messageBody.Message.ID != "message-1" {
		t.Fatalf("message response body = %+v, err=%v", messageBody, err)
	}
	if resolver.reference != "root-a:session-1" || resolver.profile != "dev" || resolver.messageID != "message-1" || resolver.cursor == nil || *resolver.cursor != cursorValue {
		t.Fatalf("message lookup = %q/%q/%q/%v", resolver.reference, resolver.profile, resolver.messageID, resolver.cursor)
	}

	invalid := httptest.NewRequest(http.MethodGet, "/api/sessions/root-a:session-1?before=-1", nil)
	invalid.Header.Set("Authorization", "Bearer "+testAPIToken)
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid pagination status = %d, body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

func TestCatalogRoutesRequireCatalogCapability(t *testing.T) {
	handler := testHandler(t, &fakeResolver{}, Config{
		APIToken:            testAPIToken,
		MaxBodyBytes:        4096,
		SSEMaxBufferedBytes: 4096,
		RunStateRetention:   time.Minute,
	})
	defer handler.Close()
	request := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "session listing is unavailable") {
		t.Fatalf("catalog-less list response = %d %s", response.Code, response.Body.String())
	}
}

type fakeCatalogResolver struct {
	*fakeResolver
	list      CatalogResponse
	snapshot  CatalogResponse
	history   CatalogResponse
	message   json.RawMessage
	reference string
	profile   string
	messageID string
	cursor    *int64
	options   SnapshotOptions
}

func (r *fakeCatalogResolver) ListSessions(_ context.Context, ifNoneMatch string, _ SessionRunProvider) (CatalogResponse, error) {
	if ETagMatches(ifNoneMatch, r.list.ETag) {
		return CatalogResponse{ETag: r.list.ETag, NotModified: true}, nil
	}
	return r.list, nil
}

func (r *fakeCatalogResolver) ReadSessionSnapshot(_ context.Context, reference, profile string, options SnapshotOptions, ifNoneMatch string, _ SessionRunProvider) (CatalogResponse, error) {
	r.reference, r.profile, r.options = reference, profile, options
	if ETagMatches(ifNoneMatch, r.snapshot.ETag) {
		return CatalogResponse{ETag: r.snapshot.ETag, NotModified: true}, nil
	}
	return r.snapshot, nil
}

func (r *fakeCatalogResolver) ReadSessionHistory(_ context.Context, reference, profile, ifNoneMatch string) (CatalogResponse, error) {
	r.reference, r.profile = reference, profile
	if ETagMatches(ifNoneMatch, r.history.ETag) {
		return CatalogResponse{ETag: r.history.ETag, NotModified: true}, nil
	}
	return r.history, nil
}

func (r *fakeCatalogResolver) ReadSessionMessage(_ context.Context, reference, profile, messageID string, cursor *int64) (json.RawMessage, error) {
	r.reference, r.profile, r.messageID, r.cursor = reference, profile, messageID, cursor
	return r.message, nil
}
