package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestRequestDraftToolRendersUpdatedAnswerAndReportsChangedPaths(t *testing.T) {
	policy := requestDraftTestSettings()
	proposed := json.RawMessage(`{"goal":"build","scope":["read"]}`)
	updated := json.RawMessage(`{"goal":"implement","scope":["read","test"]}`)
	var sawQuestion tools.InteractionQuestion
	var sawIdentity tools.InteractionIdentity
	var resolvedAnswer tools.InteractionAnswer
	var resolvedDraft json.RawMessage
	tool, err := NewRequestDraftTool(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !tool.RequiresUserDecision {
		t.Fatal("request_draft must require a user decision")
	}
	output, err := executeNativeRequestDraft(context.Background(), map[string]any{
		"title": "  Confirm implementation scope  ",
		"draft": map[string]any{"goal": "build", "scope": []any{"read"}},
	}, tools.ExecutionContext{
		CallID:       "call-17",
		ToolName:     "request_draft",
		RawArguments: json.RawMessage(`{"title":"  Confirm implementation scope  ","draft":{"goal":"build","scope":["read"]}}`),
		RequestInteraction: func(_ context.Context, identity tools.InteractionIdentity, question tools.InteractionQuestion) (tools.InteractionAnswer, error) {
			sawIdentity = identity
			sawQuestion = question
			return tools.InteractionAnswer{Decision: tools.InteractionUpdate, Outcome: tools.InteractionAnswered, Source: tools.InteractionSourceUser, Draft: updated}, nil
		},
		OnInteractionResolved: func(identity tools.InteractionIdentity, answer tools.InteractionAnswer, draft json.RawMessage) error {
			if identity.Tool != "request_draft" || identity.ToolCallID != "call-17" {
				t.Fatalf("resolved identity = %+v", identity)
			}
			resolvedAnswer = answer
			resolvedDraft = append(json.RawMessage(nil), draft...)
			return nil
		},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if sawIdentity.Tool != "request_draft" || sawIdentity.ToolCallID != "call-17" || sawQuestion.TimeoutMS != policy.TimeoutMS || sawQuestion.ExtendMS != policy.ExtendMS || sawQuestion.MaxTimeoutMS != policy.MaxTimeoutMS || sawQuestion.Title != "Confirm implementation scope" || string(sawQuestion.Draft) != string(proposed) {
		t.Fatalf("interaction request = %+v, identity %+v", sawQuestion, sawIdentity)
	}
	if resolvedAnswer.Decision != tools.InteractionUpdate || string(resolvedDraft) != string(proposed) {
		t.Fatalf("resolved answer=%+v proposed=%s", resolvedAnswer, resolvedDraft)
	}
	want := "{\n" +
		"  \"decision\": \"update\",\n" +
		"  \"status\": \"answered\",\n" +
		"  \"draft\": {\n" +
		"    \"goal\": \"implement\",\n" +
		"    \"scope\": [\n" +
		"      \"read\",\n" +
		"      \"test\"\n" +
		"    ]\n" +
		"  },\n" +
		"  \"changed\": [\n" +
		"    \"goal\",\n" +
		"    \"scope[1]\"\n" +
		"  ]\n" +
		"}"
	if output != want {
		t.Fatalf("request_draft output:\n%s\nwant:\n%s", output, want)
	}
}

func TestRequestDraftToolRefusesOversizedDeepAndLongTitle(t *testing.T) {
	policy := requestDraftTestSettings()
	policy.MaxDraftBytes = 8
	if output := requestDraftOutput(t, `{"draft":{"a":"long"}}`, map[string]any{"draft": map[string]any{"a": "long"}}, policy); !strings.Contains(output, `"error": "draft_too_large"`) || !strings.Contains(output, `"max_draft_bytes": 8`) {
		t.Fatalf("oversized draft refusal = %s", output)
	}

	policy = requestDraftTestSettings()
	policy.MaxDraftDepth = 1
	if output := requestDraftOutput(t, `{"draft":{"a":{"b":1}}}`, map[string]any{"draft": map[string]any{"a": map[string]any{"b": float64(1)}}}, policy); !strings.Contains(output, `"error": "draft_too_deep"`) || !strings.Contains(output, `"max_draft_depth": 1`) {
		t.Fatalf("deep draft refusal = %s", output)
	}

	policy = requestDraftTestSettings()
	policy.MaxTitleChars = 1
	if output := requestDraftOutput(t, `{"title":"😀","draft":{}}`, map[string]any{"title": "😀", "draft": map[string]any{}}, policy); !strings.Contains(output, `"error": "title_too_long"`) || !strings.Contains(output, `title is 2 characters`) {
		t.Fatalf("long title refusal = %s", output)
	}
}

func TestRequestDraftToolHeadlessTimeoutAndCancellation(t *testing.T) {
	policy := requestDraftTestSettings()
	policy.HeadlessAutoAcceptTimeoutMS = 1
	output := requestDraftOutput(t, `{"draft":{"goal":"ship"}}`, map[string]any{"draft": map[string]any{"goal": "ship"}}, policy)
	if !strings.Contains(output, `"decision": "accept"`) || !strings.Contains(output, `"status": "answered"`) {
		t.Fatalf("headless timeout result = %s", output)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	arguments := map[string]any{"draft": map[string]any{"goal": "ship"}}
	result, err := executeNativeRequestDraft(ctx, arguments, tools.ExecutionContext{RawArguments: json.RawMessage(`{"draft":{"goal":"ship"}}`)}, requestDraftTestSettings())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"status": "cancelled"`) || !strings.Contains(result, "The run was stopped before the draft was answered.") {
		t.Fatalf("cancelled headless result = %s", result)
	}
}

func TestRequestDraftCatalogSchemaAndUserDecisionRegistration(t *testing.T) {
	tool, err := NewRequestDraftTool(requestDraftTestSettings())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.IsUserDecisionCall(json.RawMessage(`{"name":"request_draft"}`)) {
		t.Fatal("catalog did not recognize request_draft as a user decision")
	}
	if _, err := catalog.ExecuteFunctionCall(context.Background(), json.RawMessage(`{"type":"function_call","call_id":"draft-1","name":"request_draft","arguments":"{\"draft\":{\"goal\":\"ship\"}}"}`), tools.ExecutionContext{
		RequestInteraction: func(context.Context, tools.InteractionIdentity, tools.InteractionQuestion) (tools.InteractionAnswer, error) {
			return tools.InteractionAnswer{Decision: tools.InteractionAccept, Outcome: tools.InteractionAnswered, Source: tools.InteractionSourceUser}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRenderRequestDraftAnswerUsesJSONStringifyEscapingAndKeyOrder(t *testing.T) {
	answer := tools.InteractionAnswer{
		Decision: tools.InteractionUpdate,
		Outcome:  tools.InteractionAnswered,
		Draft:    json.RawMessage(`{"b":"new","10":"ten","2":"two","x":"\u0061","literal":"\\u0062"}`),
	}
	proposed := json.RawMessage(`{"2":"two","10":"ten","b":"old","x":"","literal":""}`)
	output, err := renderRequestDraftAnswer(answer, proposed)
	if err != nil {
		t.Fatal(err)
	}
	want := `{` + "\n" +
		`  "decision": "update",` + "\n" +
		`  "status": "answered",` + "\n" +
		`  "draft": {` + "\n" +
		`    "2": "two",` + "\n" +
		`    "10": "ten",` + "\n" +
		`    "b": "new",` + "\n" +
		`    "x": "a",` + "\n" +
		`    "literal": "\\u0062"` + "\n" +
		`  },` + "\n" +
		`  "changed": [` + "\n" +
		`    "b",` + "\n" +
		`    "x",` + "\n" +
		`    "literal"` + "\n" +
		`  ]` + "\n" +
		`}`
	if output != want {
		t.Fatalf("rendered JSON = %s\nwant = %s", output, want)
	}
}

func requestDraftOutput(t *testing.T, raw string, arguments map[string]any, policy sessionfs.NativeRequestDraftSettings) string {
	t.Helper()
	output, err := executeNativeRequestDraft(context.Background(), arguments, tools.ExecutionContext{RawArguments: json.RawMessage(raw)}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func requestDraftTestSettings() sessionfs.NativeRequestDraftSettings {
	return sessionfs.NativeRequestDraftSettings{
		TimeoutMS: 5_000, HeadlessAutoAcceptTimeoutMS: 1, MaxTimeoutMS: 10_000, ExtendMS: 2_000,
		MaxDraftBytes: 32 * 1024, MaxDraftDepth: 8, MaxTitleChars: 200,
	}
}
