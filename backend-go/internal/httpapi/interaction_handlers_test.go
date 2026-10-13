package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/tools"
)

func TestRequestDraftInteractionHTTPReplayExtendAndAnswer(t *testing.T) {
	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"resp_draft","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"approved"}]}]}}`)
	answers := make(chan tools.InteractionAnswer, 1)
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		if options.ToolExecution.RequestInteraction == nil {
			return session.RunResult{}, fmt.Errorf("run did not expose request_interaction")
		}
		answer, err := options.ToolExecution.RequestInteraction(ctx, tools.InteractionIdentity{Tool: "request_draft", ToolCallID: "call-draft-1"}, tools.InteractionQuestion{
			Draft: json.RawMessage(`{"goal":"ship"}`), Title: "Confirm scope", TimeoutMS: 5_000, ExtendMS: 5_000, MaxTimeoutMS: 10_000,
			Limits: tools.InteractionLimits{MaxDraftBytes: 1024, MaxDraftDepth: 8},
		})
		if err != nil {
			return session.RunResult{}, err
		}
		answers <- answer
		return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-draft", Profile: "dev", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 16 * 1024, RunStateRetention: time.Minute,
	})
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()

	startRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/session-draft/run?profile=dev", strings.NewReader(`{"input":"clarify","supports_interaction":true}`))
	startRequest.Header.Set("Authorization", "Bearer "+testAPIToken)
	startRequest.Header.Set("Content-Type", "application/json")
	startResponse, err := client.Do(startRequest)
	if err != nil {
		t.Fatal(err)
	}
	if startResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(startResponse.Body)
		t.Fatalf("POST run status=%d body=%s", startResponse.StatusCode, body)
	}
	initialEvents := make(chan parsedEvent, 32)
	streamDone := make(chan error, 1)
	go func() {
		defer startResponse.Body.Close()
		reader := bufio.NewReader(startResponse.Body)
		for {
			event, err := readSSEEvent(reader)
			if err != nil {
				if err == io.EOF {
					streamDone <- nil
				} else {
					streamDone <- err
				}
				return
			}
			initialEvents <- event
			if event.name == "done" {
				streamDone <- nil
				return
			}
		}
	}()

	var pending tools.PendingInteraction
	for {
		select {
		case event := <-initialEvents:
			if event.name == "interaction" {
				var payload struct {
					Pending *tools.PendingInteraction `json:"pending"`
				}
				if err := json.Unmarshal(event.data, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Pending != nil {
					pending = *payload.Pending
					goto havePending
				}
			}
		case err := <-streamDone:
			t.Fatalf("run stream ended before interaction appeared: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for request_draft interaction")
		}
	}

havePending:
	if pending.Tool != "request_draft" || pending.ToolCallID != "call-draft-1" || pending.Title != "Confirm scope" {
		t.Fatalf("pending interaction = %+v", pending)
	}

	attachRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/api/sessions/session-draft/run/stream?profile=dev", nil)
	attachRequest.Header.Set("Authorization", "Bearer "+testAPIToken)
	attachResponse, err := client.Do(attachRequest)
	if err != nil {
		t.Fatal(err)
	}
	var replayed bool
	attachReader := bufio.NewReader(attachResponse.Body)
	for index := 0; index < 5; index++ {
		event, err := readSSEEvent(attachReader)
		if err != nil {
			t.Fatalf("read reattachment event: %v", err)
		}
		if event.name == "interaction" {
			var payload struct {
				Pending *tools.PendingInteraction `json:"pending"`
			}
			if err := json.Unmarshal(event.data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Pending == nil || payload.Pending.ID != pending.ID {
				t.Fatalf("reattached pending = %+v", payload.Pending)
			}
			replayed = true
			break
		}
	}
	_ = attachResponse.Body.Close()
	if !replayed {
		t.Fatal("reattached stream did not replay pending interaction")
	}

	extendBody := `{"interaction_id":"` + pending.ID + `"}`
	extendRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/session-draft/run/interaction/extend?profile=dev", strings.NewReader(extendBody))
	extendRequest.Header.Set("Authorization", "Bearer "+testAPIToken)
	extendRequest.Header.Set("Content-Type", "application/json")
	extendResponse, err := client.Do(extendRequest)
	if err != nil {
		t.Fatal(err)
	}
	var extended struct {
		Interaction struct {
			InteractionID string `json:"interaction_id"`
			Status        string `json:"status"`
			ExpiresAt     string `json:"expires_at"`
			MaxExpiresAt  string `json:"max_expires_at"`
		} `json:"interaction"`
	}
	decodeResponse(t, extendResponse, &extended)
	if extended.Interaction.InteractionID != pending.ID || extended.Interaction.Status != "pending" || extended.Interaction.ExpiresAt == pending.ExpiresAt || extended.Interaction.MaxExpiresAt != pending.MaxExpiresAt {
		t.Fatalf("extend response = %+v", extended.Interaction)
	}

	answerBody := `{"interaction_id":"` + pending.ID + `","decision":"update","draft":{"goal":"ship safely"}}`
	answerRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/session-draft/run/interaction?profile=dev", strings.NewReader(answerBody))
	answerRequest.Header.Set("Authorization", "Bearer "+testAPIToken)
	answerRequest.Header.Set("Content-Type", "application/json")
	answerResponse, err := client.Do(answerRequest)
	if err != nil {
		t.Fatal(err)
	}
	var answerResult struct {
		Interaction struct {
			InteractionID string `json:"interaction_id"`
			Decision      string `json:"decision"`
			Status        string `json:"status"`
		} `json:"interaction"`
	}
	decodeResponse(t, answerResponse, &answerResult)
	if answerResult.Interaction.InteractionID != pending.ID || answerResult.Interaction.Decision != "update" || answerResult.Interaction.Status != "answered" {
		t.Fatalf("answer response = %+v", answerResult.Interaction)
	}
	select {
	case answer := <-answers:
		if answer.Decision != tools.InteractionUpdate || answer.Outcome != tools.InteractionAnswered || string(answer.Draft) != `{"goal":"ship safely"}` {
			t.Fatalf("runner received answer = %+v", answer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not receive answer")
	}
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run stream did not finish after answer")
	}
}

func readSSEEvent(reader *bufio.Reader) (parsedEvent, error) {
	var event parsedEvent
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return parsedEvent{}, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if event.name != "" {
				return event, nil
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			event.name = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			event.data = append(event.data[:0], strings.TrimPrefix(line, "data: ")...)
		}
	}
}

func decodeResponse(t *testing.T, response *http.Response, value any) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("response status=%d body=%s", response.StatusCode, body)
	}
	if err := json.NewDecoder(response.Body).Decode(value); err != nil {
		t.Fatal(err)
	}
}
