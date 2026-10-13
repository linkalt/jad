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

func parseReply(t *testing.T, body string, kind tools.InteractionKind) (tools.InteractionReply, error) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	_, reply, err := parseInteractionReply(fields, nil, kind)
	return reply, err
}

func TestParseInteractionReplyRefusesUpdateOnlyForAnApproval(t *testing.T) {
	update := `{"interaction_id":"i","decision":"update","draft":{"command":"rm -rf /"}}`
	if _, err := parseReply(t, update, tools.InteractionKindApproval); err == nil || !strings.Contains(err.Error(), `"accept" or "decline"`) {
		t.Fatalf("update on an approval: error = %v", err)
	}
	if reply, err := parseReply(t, update, tools.InteractionKindDraft); err != nil || reply.Decision != tools.InteractionUpdate {
		t.Fatalf("update on a draft must still work: %+v, %v", reply, err)
	}
	if reply, err := parseReply(t, `{"interaction_id":"i","decision":"accept"}`, tools.InteractionKindApproval); err != nil || reply.Decision != tools.InteractionAccept {
		t.Fatalf("accept on an approval: %+v, %v", reply, err)
	}
	// A refusal still needs a reason, so the model has something to re-plan from.
	if _, err := parseReply(t, `{"interaction_id":"i","decision":"decline"}`, tools.InteractionKindApproval); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("decline without a reason: error = %v", err)
	}
	if reply, err := parseReply(t, `{"interaction_id":"i","decision":"decline","reason":"  not now  "}`, tools.InteractionKindApproval); err != nil || reply.Reason == nil || *reply.Reason != "not now" {
		t.Fatalf("decline with a reason: %+v, %v", reply, err)
	}
}

func TestPendingToWebUICarriesTheKindOnlyForAnApproval(t *testing.T) {
	draft, _ := json.Marshal(pendingToWebUI(tools.PendingInteraction{ID: "a", Draft: json.RawMessage(`{}`)}))
	if strings.Contains(string(draft), `"kind"`) {
		t.Fatalf("a draft's message mirror carries a kind: %s", draft)
	}
	approval, _ := json.Marshal(pendingToWebUI(tools.PendingInteraction{ID: "a", Kind: tools.InteractionKindApproval, Draft: json.RawMessage(`{}`)}))
	if !strings.Contains(string(approval), `"kind":"approval"`) {
		t.Fatalf("an approval's message mirror does not say so: %s", approval)
	}
}

func TestApprovalQuestionOverHTTPIsPublishedWithItsKindAndRefusesUpdate(t *testing.T) {
	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"resp_a","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`)
	answers := make(chan tools.InteractionAnswer, 1)
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		if options.ToolExecution.RequestInteraction == nil {
			return session.RunResult{}, fmt.Errorf("run did not expose request_interaction")
		}
		answer, err := options.ToolExecution.RequestInteraction(ctx, tools.InteractionIdentity{Tool: "bash", ToolCallID: "call-1"}, tools.InteractionQuestion{
			Kind: tools.InteractionKindApproval, Draft: json.RawMessage(`{"tool":"bash","command":"ls"}`), Title: "Run shell command",
			TimeoutMS: 5_000, ExtendMS: 5_000, MaxTimeoutMS: 10_000,
		})
		if err != nil {
			return session.RunResult{}, err
		}
		answers <- answer
		return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-approval", Profile: "dev", Runner: runner}}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 16 * 1024, RunStateRetention: time.Minute})
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()

	start, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/session-approval/run?profile=dev", strings.NewReader(`{"input":"list","supports_interaction":true}`))
	start.Header.Set("Authorization", "Bearer "+testAPIToken)
	start.Header.Set("Content-Type", "application/json")
	response, err := client.Do(start)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)

	// The run has published no message yet, so there is nothing to mirror the
	// question onto; the dedicated event is what a client reads here.
	events := make(chan parsedEvent, 16)
	go func() {
		for {
			event, err := readSSEEvent(reader)
			if err != nil {
				return
			}
			events <- event
		}
	}()
	var pendingID, eventKind string
	deadline := time.After(3 * time.Second)
	for pendingID == "" {
		select {
		case event := <-events:
			if event.name != "interaction" {
				continue
			}
			var payload struct {
				Pending *struct {
					ID   string `json:"id"`
					Kind string `json:"kind"`
				} `json:"pending"`
			}
			if err := json.Unmarshal(event.data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Pending != nil {
				pendingID, eventKind = payload.Pending.ID, payload.Pending.Kind
			}
		case <-deadline:
			t.Fatal("timed out waiting for the approval question")
		}
	}
	if eventKind != "approval" {
		t.Fatalf("kind on the interaction event = %q, want approval", eventKind)
	}

	post := func(body string) (int, string) {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/session-approval/run/interaction?profile=dev", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testAPIToken)
		request.Header.Set("Content-Type", "application/json")
		reply, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer reply.Body.Close()
		raw, _ := io.ReadAll(reply.Body)
		return reply.StatusCode, string(raw)
	}
	status, body := post(`{"interaction_id":"` + pendingID + `","decision":"update","draft":{"command":"rm -rf /"}}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "approval") {
		t.Fatalf("update on an approval: status %d body %s, want 400 naming approval", status, body)
	}
	select {
	case answer := <-answers:
		t.Fatalf("a refused update settled the question: %+v", answer)
	case <-time.After(150 * time.Millisecond):
	}
	status, body = post(`{"interaction_id":"` + pendingID + `","decision":"accept"}`)
	if status != http.StatusOK {
		t.Fatalf("accept: status %d body %s", status, body)
	}
	select {
	case answer := <-answers:
		if answer.Decision != tools.InteractionAccept || answer.Outcome != tools.InteractionAnswered {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the accept did not reach the run")
	}
}

// A question's end is sent once, on the event that clears it, and says why. The run
// here asks one approval; the three cases answer it, deny it and stop the run.
func TestTheClearingEventSaysWhyAnApprovalEnded(t *testing.T) {
	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"resp_a","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`)
	cases := []struct {
		name string
		act  func(post func(path, body string) int, id string) int
		want map[string]any
	}{
		{"allowed", func(post func(string, string) int, id string) int {
			return post("/run/interaction", `{"interaction_id":"`+id+`","decision":"accept"}`)
		}, map[string]any{"kind": "approval", "tool": "bash", "outcome": "answered", "decision": "accept"}},
		{"denied", func(post func(string, string) int, id string) int {
			return post("/run/interaction", `{"interaction_id":"`+id+`","decision":"decline","reason":"no"}`)
		}, map[string]any{"kind": "approval", "tool": "bash", "outcome": "answered", "decision": "decline"}},
		{"stopped", func(post func(string, string) int, id string) int {
			return post("/run/stop", `{}`)
		}, map[string]any{"kind": "approval", "tool": "bash", "outcome": "cancelled"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
				if _, err := options.ToolExecution.RequestInteraction(ctx, tools.InteractionIdentity{Tool: "bash", ToolCallID: "call-1"}, tools.InteractionQuestion{
					Kind: tools.InteractionKindApproval, Draft: json.RawMessage(`{"command":"ls"}`), TimeoutMS: 5_000, MaxTimeoutMS: 5_000,
				}); err != nil {
					return session.RunResult{}, err
				}
				if ctx.Err() != nil {
					return session.RunResult{}, ctx.Err()
				}
				return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
			})
			resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-end", Profile: "dev", Runner: runner}}
			handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 16 * 1024, RunStateRetention: time.Minute})
			defer handler.Close()
			server := httptest.NewServer(handler)
			defer server.Close()
			client := server.Client()

			start, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/session-end/run?profile=dev", strings.NewReader(`{"input":"list","supports_interaction":true}`))
			start.Header.Set("Authorization", "Bearer "+testAPIToken)
			start.Header.Set("Content-Type", "application/json")
			response, err := client.Do(start)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			reader := bufio.NewReader(response.Body)
			events := make(chan parsedEvent, 32)
			go func() {
				for {
					event, err := readSSEEvent(reader)
					if err != nil {
						close(events)
						return
					}
					events <- event
				}
			}()
			post := func(path, body string) int {
				request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/session-end"+path+"?profile=dev", strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer "+testAPIToken)
				request.Header.Set("Content-Type", "application/json")
				reply, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer reply.Body.Close()
				io.Copy(io.Discard, reply.Body)
				return reply.StatusCode
			}

			var pendingID string
			var ends []map[string]any
			var acted bool
			deadline := time.After(5 * time.Second)
		loop:
			for {
				select {
				case event, ok := <-events:
					if !ok {
						break loop
					}
					if event.name == "done" {
						break loop
					}
					if event.name != "interaction" {
						continue
					}
					var payload struct {
						Pending *struct {
							ID string `json:"id"`
						} `json:"pending"`
						Ended map[string]any `json:"ended"`
					}
					if err := json.Unmarshal(event.data, &payload); err != nil {
						t.Fatal(err)
					}
					if payload.Pending != nil {
						pendingID = payload.Pending.ID
						if !acted {
							acted = true
							if status := tc.act(post, pendingID); status != http.StatusOK {
								t.Fatalf("%s: status %d", tc.name, status)
							}
						}
						continue
					}
					ends = append(ends, payload.Ended)
				case <-deadline:
					t.Fatal("timed out reading the run")
				}
			}
			want := map[string]any{"interaction_id": pendingID}
			for key, value := range tc.want {
				want[key] = value
			}
			if len(ends) != 1 {
				t.Fatalf("%s: the question ended %d times: %v", tc.name, len(ends), ends)
			}
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(ends[0])
			if string(wantJSON) != string(gotJSON) {
				t.Fatalf("%s: ended = %s, want %s", tc.name, gotJSON, wantJSON)
			}
		})
	}
}
