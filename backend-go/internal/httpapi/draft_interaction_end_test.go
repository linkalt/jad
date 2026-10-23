package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/tools"
)

// A question's end is sent once, on the event that clears it, and says why. The run
// here asks one draft; the three cases answer it, deny it and stop the run.
func TestTheClearingEventSaysWhyADraftEnded(t *testing.T) {
	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"resp_a","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`)
	cases := []struct {
		name string
		act  func(post func(path, body string) int, id string) int
		want map[string]any
	}{
		{"allowed", func(post func(string, string) int, id string) int {
			return post("/run/interaction", `{"interaction_id":"`+id+`","decision":"accept"}`)
		}, map[string]any{"tool": "bash", "outcome": "answered", "decision": "accept"}},
		{"denied", func(post func(string, string) int, id string) int {
			return post("/run/interaction", `{"interaction_id":"`+id+`","decision":"decline","reason":"no"}`)
		}, map[string]any{"tool": "bash", "outcome": "answered", "decision": "decline"}},
		{"stopped", func(post func(string, string) int, id string) int {
			return post("/run/stop", `{}`)
		}, map[string]any{"tool": "bash", "outcome": "cancelled"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
				if _, err := options.ToolExecution.RequestInteraction(ctx, tools.InteractionIdentity{Tool: "bash", ToolCallID: "call-1"}, tools.InteractionQuestion{
					Draft: json.RawMessage(`{"command":"ls"}`), TimeoutMS: 5_000, MaxTimeoutMS: 5_000,
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
