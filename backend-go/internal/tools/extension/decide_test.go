package extension

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// decideRig is a decide tool wired to a fake provider. Requests records each request
// the provider received.
type decideRig struct {
	catalog  *tools.Catalog
	settings sessionfs.ExtensionDecideSettings
	requests []decideRecorded
	server   *httptest.Server
}

type decideRecorded struct {
	Method, Path, Authorization string
	Body                        map[string]any
}

func newDecideRig(t *testing.T, backend string, status int, reply string, adjust func(*sessionfs.ExtensionDecideSettings)) *decideRig {
	t.Helper()
	rig := &decideRig{}
	rig.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		recorded := decideRecorded{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization")}
		_ = json.Unmarshal(data, &recorded.Body)
		rig.requests = append(rig.requests, recorded)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(rig.server.Close)
	rig.settings = sessionfs.ExtensionDecideSettings{Backend: backend, BaseURL: rig.server.URL, APIKeyEnv: "DECIDE_KEY", Model: "m-1", TimeoutMS: 5000, MaxQuestions: 4, MaxInputBytes: 1000, MaxResponseBytes: 4096}
	if adjust != nil {
		adjust(&rig.settings)
	}
	environment := func(name string) string {
		if name == "DECIDE_KEY" {
			return " secret-key "
		}
		return ""
	}
	tool, err := newDecideTool(rig.settings, DecideImages{}, environment, rig.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	rig.catalog = catalog
	return rig
}

func (r *decideRig) call(t *testing.T, arguments string) string {
	t.Helper()
	return executeCatalogCall(t, r.catalog, "decide", arguments)
}

// content returns the JSON body of an ok result.
func decideContent(t *testing.T, output string) map[string]any {
	t.Helper()
	index := strings.Index(output, `{"answers"`)
	if index < 0 {
		t.Fatalf("no answers in:\n%s", output)
	}
	text := output[index:]
	if end := strings.Index(text, "\n"); end >= 0 {
		text = text[:end]
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("answers are not JSON: %v\n%s", err, text)
	}
	return parsed
}

const openRouterReply = `{"model":"x","answers":{
 "billing":{"type":"noul","noul":0.9234567},
 "team":{"type":"choice","choice":"billing","probabilities":{"billing":0.97,"technical":0.02,"other":0.01},"confidence":0.97},
 "severity":{"type":"score","score":1.6,"legend":{"0":"a","1":"b","2":"c"},"probabilities":{"0":0.1,"1":0.2,"2":0.7},"confidence":0.7}},
 "usage":{"input_tokens":3}}`

const mixedQuestions = `{"input":"I was charged twice.","questions":[
 {"name":"billing","instructions":"Is this a billing problem?"},
 {"name":"team","instructions":"Who handles it?","choices":[{"value":"billing","description":"Payments."},"technical","other"]},
 {"name":"severity","instructions":"How severe?","levels":[{"label":"Cosmetic","description":"Looks only."},"Workaround","Blocked"]}]}`

func TestDecideOpenRouterRequestAndAnswers(t *testing.T) {
	rig := newDecideRig(t, "openrouter", 200, openRouterReply, nil)
	output := rig.call(t, mixedQuestions)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "Answered 3 questions") {
		t.Fatalf("not ok:\n%s", output)
	}
	if len(rig.requests) != 1 {
		t.Fatalf("%d requests", len(rig.requests))
	}
	request := rig.requests[0]
	if request.Method != "POST" || request.Path != "/decisions" || request.Authorization != "Bearer secret-key" {
		t.Fatalf("request = %+v", request)
	}
	if request.Body["model"] != "m-1" || request.Body["state"] != "I was charged twice." {
		t.Fatalf("body = %#v", request.Body)
	}
	questions := request.Body["questions"].(map[string]any)
	billing := questions["billing"].(map[string]any)
	if billing["type"] != "noul" || billing["instructions"] != "Is this a billing problem?" {
		t.Fatalf("predicate sent as %#v", billing)
	}
	team := questions["team"].(map[string]any)
	criteria := team["criteria"].(map[string]any)
	if team["type"] != "choice" || criteria["billing"] != "Payments." || criteria["technical"] != "technical" || len(criteria) != 3 {
		t.Fatalf("choice sent as %#v", team)
	}
	severity := questions["severity"].(map[string]any)
	levels := severity["criteria"].([]any)
	if severity["type"] != "score" || len(levels) != 3 || levels[0] != "Cosmetic: Looks only." || levels[1] != "Workaround" {
		t.Fatalf("score sent as %#v", severity)
	}

	answers := decideContent(t, output)["answers"].([]any)
	if len(answers) != 3 {
		t.Fatalf("answers = %#v", answers)
	}
	first := answers[0].(map[string]any)
	if first["name"] != "billing" || first["type"] != "predicate" || first["probability"] != 0.9235 {
		t.Fatalf("predicate answer = %#v", first)
	}
	second := answers[1].(map[string]any)
	if second["choice"] != "billing" || second["confidence"] != 0.97 || second["probabilities"].(map[string]any)["technical"] != 0.02 {
		t.Fatalf("choice answer = %#v", second)
	}
	third := answers[2].(map[string]any)
	// 1.6 rounds to level index 2 (Blocked); probabilities are shown by label.
	if third["score"] != 1.6 || third["level"] != "Blocked" || third["probabilities"].(map[string]any)["Cosmetic"] != 0.1 {
		t.Fatalf("score answer = %#v", third)
	}
	if !strings.Contains(output, `{"name":"billing","type":"predicate","probability":0.9235}`) {
		t.Fatalf("answer field order changed:\n%s", output)
	}
}

const openAIReply = `{"model":"gpt-6-luna","usage":{"input_tokens":3},"answers":[
 {"type":"predicate","name":"billing","probability":0.12},
 {"type":"choice","name":"team","choice":"other","confidence":0.5,"probabilities":[{"value":"billing","probability":0.3},{"value":"technical","probability":0.2},{"value":"other","probability":0.5}]},
 {"type":"score","name":"severity","score":0.2,"confidence":0.8,"probabilities":[{"label":"Cosmetic","value":0,"probability":0.8},{"label":"Workaround","value":1,"probability":0.2},{"label":"Blocked","value":2,"probability":0}]}]}`

func TestDecideOpenAIRequestAndAnswers(t *testing.T) {
	rig := newDecideRig(t, "openai", 200, openAIReply, nil)
	output := rig.call(t, mixedQuestions)
	if !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("not ok:\n%s", output)
	}
	request := rig.requests[0]
	if request.Path != "/decisions" || request.Body["input"] != "I was charged twice." || request.Body["model"] != "m-1" {
		t.Fatalf("request = %+v", request)
	}
	questions := request.Body["questions"].([]any)
	if len(questions) != 3 {
		t.Fatalf("questions = %#v", questions)
	}
	predicate := questions[0].(map[string]any)
	if predicate["type"] != "predicate" || predicate["name"] != "billing" {
		t.Fatalf("predicate sent as %#v", predicate)
	}
	choice := questions[1].(map[string]any)
	choices := choice["choices"].([]any)
	if choice["type"] != "choice" || len(choices) != 3 || choices[0].(map[string]any)["description"] != "Payments." || choices[1].(map[string]any)["value"] != "technical" {
		t.Fatalf("choice sent as %#v", choice)
	}
	if _, has := choices[1].(map[string]any)["description"]; has {
		t.Fatalf("an empty description was sent: %#v", choices[1])
	}
	level := questions[2].(map[string]any)["levels"].([]any)[0].(map[string]any)
	if level["label"] != "Cosmetic" || level["description"] != "Looks only." {
		t.Fatalf("level sent as %#v", level)
	}

	answers := decideContent(t, output)["answers"].([]any)
	if answers[0].(map[string]any)["probability"] != 0.12 || answers[1].(map[string]any)["choice"] != "other" {
		t.Fatalf("answers = %#v", answers)
	}
	score := answers[2].(map[string]any)
	if score["level"] != "Cosmetic" || score["probabilities"].(map[string]any)["Workaround"] != 0.2 {
		t.Fatalf("score answer = %#v", score)
	}
}

func TestDecideOpenAIRefusalWithoutNameFollowsQuestionOrder(t *testing.T) {
	reply := `{"answers":[{"type":"refusal"},{"type":"predicate","name":"b","probability":1}]}`
	rig := newDecideRig(t, "openai", 200, reply, nil)
	output := rig.call(t, `{"input":"x","questions":[{"name":"a","instructions":"A?"},{"name":"b","instructions":"B?"}]}`)
	answers := decideContent(t, output)["answers"].([]any)
	if answers[0].(map[string]any)["status"] != "refused" || answers[1].(map[string]any)["probability"] != 1.0 {
		t.Fatalf("answers = %#v", answers)
	}
	if _, has := answers[0].(map[string]any)["probability"]; has {
		t.Fatal("a refusal must not carry a probability the model could read as 0")
	}
}

func TestDecideMarksAQuestionTheProviderDidNotAnswer(t *testing.T) {
	rig := newDecideRig(t, "openrouter", 200, `{"answers":{"a":{"type":"noul","noul":0}}}`, nil)
	output := rig.call(t, `{"input":"x","questions":[{"name":"a","instructions":"A?"},{"name":"b","instructions":"B?"}]}`)
	answers := decideContent(t, output)["answers"].([]any)
	if answers[1].(map[string]any)["status"] != "missing" {
		t.Fatalf("a gap must be reported, not shown as a negative: %#v", answers[1])
	}
	if answers[0].(map[string]any)["probability"] != 0.0 {
		t.Fatalf("a real zero must stay a zero: %#v", answers[0])
	}
}

func TestDecideSmallestCallAndNormalizedArguments(t *testing.T) {
	rig := newDecideRig(t, "openrouter", 200, `{"answers":{"q1":{"type":"noul","noul":1},"q2":{"type":"noul","noul":0}}}`, nil)
	// No name, no type; "NOUL" and padding are repaired instead of rejected.
	output := rig.call(t, `{"input":"x","questions":[{"instructions":"  Is it x?  "},{"type":" NOUL ","instructions":"Is it y?"}]}`)
	if !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("not ok:\n%s", output)
	}
	questions := rig.requests[0].Body["questions"].(map[string]any)
	if questions["q1"].(map[string]any)["instructions"] != "Is it x?" || questions["q2"].(map[string]any)["type"] != "noul" {
		t.Fatalf("questions sent = %#v", questions)
	}
	answers := decideContent(t, output)["answers"].([]any)
	if answers[0].(map[string]any)["name"] != "q1" || answers[1].(map[string]any)["type"] != "predicate" {
		t.Fatalf("answers = %#v", answers)
	}
}

func TestDecideRefusesBadQuestionsBeforeAnyRequest(t *testing.T) {
	rig := newDecideRig(t, "openrouter", 200, `{"answers":{}}`, nil)
	for name, tc := range map[string]struct{ arguments, want string }{
		"choice without choices":  {`{"input":"x","questions":[{"name":"a","type":"choice","instructions":"?"}]}`, `question "a" is a choice and needs choices`},
		"score without levels":    {`{"input":"x","questions":[{"name":"a","type":"score","instructions":"?"}]}`, `question "a" is a score and needs levels`},
		"predicate with choices":  {`{"input":"x","questions":[{"name":"a","type":"predicate","instructions":"?","choices":["a","b"]}]}`, `question "a" is a predicate and takes no choices or levels`},
		"choice with levels":      {`{"input":"x","questions":[{"name":"a","type":"choice","instructions":"?","levels":["a","b"]}]}`, `question "a" is a choice and cannot have levels`},
		"both lists":              {`{"input":"x","questions":[{"name":"a","instructions":"?","choices":["a","b"],"levels":["a","b"]}]}`, `has both choices and levels`},
		"duplicate name":          {`{"input":"x","questions":[{"name":"a","instructions":"?"},{"name":"a","instructions":"?"}]}`, `question name "a" is used twice`},
		"name collides with auto": {`{"input":"x","questions":[{"instructions":"?"},{"name":"q1","instructions":"?"}]}`, `question name "q1" is used twice`},
		"duplicate choice":        {`{"input":"x","questions":[{"name":"a","instructions":"?","choices":["x","x"]}]}`, `lists "x" twice`},
		"blank choice":            {`{"input":"x","questions":[{"name":"a","instructions":"?","choices":["x","  "]}]}`, `option without a value`},
		"blank level label":       {`{"input":"x","questions":[{"name":"a","instructions":"?","levels":["x",{"label":" "}]}]}`, `option without a label`},
		"too many questions":      {`{"input":"x","questions":[{"instructions":"1"},{"instructions":"2"},{"instructions":"3"},{"instructions":"4"},{"instructions":"5"}]}`, `5 questions; the limit is 4`},
		"input too large":         {`{"input":"` + strings.Repeat("x", 1001) + `","questions":[{"instructions":"?"}]}`, `Input is 1001 bytes; the limit is 1000`},
	} {
		t.Run(name, func(t *testing.T) {
			// The summary is JSON, so a quote in it is escaped.
			output := strings.ReplaceAll(rig.call(t, tc.arguments), `\"`, `"`)
			if !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, tc.want) {
				t.Fatalf("want invalid_input containing %q, got:\n%s", tc.want, output)
			}
		})
	}
	if len(rig.requests) != 0 {
		t.Fatalf("a bad call still sent %d request(s)", len(rig.requests))
	}
}

func TestDecideSchemaRejectsMissingParts(t *testing.T) {
	rig := newDecideRig(t, "openrouter", 200, `{"answers":{}}`, nil)
	for name, arguments := range map[string]string{
		"no input":        `{"questions":[{"instructions":"?"}]}`,
		"no questions":    `{"input":"x"}`,
		"empty questions": `{"input":"x","questions":[]}`,
		"no instructions": `{"input":"x","questions":[{"name":"a"}]}`,
		"unknown field":   `{"input":"x","questions":[{"instructions":"?"}],"model":"other"}`,
		"unknown type":    `{"input":"x","questions":[{"type":"rank","instructions":"?"}]}`,
		"one choice":      `{"input":"x","questions":[{"instructions":"?","choices":["only"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if output := rig.call(t, arguments); !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, "schema") {
				t.Fatalf("want a schema error, got:\n%s", output)
			}
		})
	}
	if len(rig.requests) != 0 {
		t.Fatalf("%d request(s) sent", len(rig.requests))
	}
}

func TestDecideReportsProviderFailures(t *testing.T) {
	const call = `{"input":"x","questions":[{"instructions":"?"}]}`
	for name, tc := range map[string]struct {
		backend, reply string
		status         int
		code, state    string
	}{
		"unauthorized":     {"openrouter", `{"error":{"message":"Missing Authentication header","code":401}}`, 401, "unauthorized", "error"},
		"forbidden":        {"openai", `{"error":{"message":"no access"}}`, 403, "unauthorized", "error"},
		"rate limited":     {"openai", `{"error":{"message":"slow down"}}`, 429, "rate_limited", "error"},
		"rejected request": {"openrouter", `{"error":{"message":"[{\"code\":\"invalid_union\"}]","code":400}}`, 400, "request_rejected", "invalid_input"},
		"server error":     {"openai", `upstream down`, 502, "provider_error", "error"},
		"plain error text": {"openai", `{"error":"bad thing"}`, 500, "provider_error", "error"},
		"not json":         {"openrouter", `<html>`, 200, "bad_response", "error"},
		"no answers":       {"openai", `{"model":"x"}`, 200, "bad_response", "error"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideRig(t, tc.backend, tc.status, tc.reply, nil)
			output := rig.call(t, call)
			if !strings.Contains(output, `"status":"`+tc.state+`"`) || !strings.Contains(output, `error_code: `+tc.code) {
				t.Fatalf("want %s/%s, got:\n%s", tc.state, tc.code, output)
			}
		})
	}
}

func TestDecideErrorTextIsOneBoundedLineAndNeverHoldsTheKey(t *testing.T) {
	long := `{"error":{"message":"` + strings.Repeat("line one\\n", 200) + `"}}`
	rig := newDecideRig(t, "openai", 400, long, nil)
	output := rig.call(t, `{"input":"x","questions":[{"instructions":"?"}]}`)
	reason := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "reason: ") {
			reason = line
		}
	}
	if reason == "" || len(reason) > 420 || !strings.HasSuffix(reason, "...") {
		t.Fatalf("reason = %q (%d bytes)", reason, len(reason))
	}
	if strings.Contains(output, "secret-key") {
		t.Fatal("the API key leaked into the result")
	}
}

func TestDecideRefusesAnOversizedResponse(t *testing.T) {
	rig := newDecideRig(t, "openai", 200, `{"answers":[],"pad":"`+strings.Repeat("x", 5000)+`"}`, nil)
	output := rig.call(t, `{"input":"x","questions":[{"instructions":"?"}]}`)
	if !strings.Contains(output, "error_code: bad_response") || !strings.Contains(output, "larger than 4096 bytes") {
		t.Fatalf("got:\n%s", output)
	}
}

func TestDecideMissingKeyDoesNotCallTheProvider(t *testing.T) {
	rig := newDecideRig(t, "openai", 200, openAIReply, func(s *sessionfs.ExtensionDecideSettings) { s.APIKeyEnv = "UNSET_KEY" })
	output := rig.call(t, `{"input":"x","questions":[{"instructions":"?"}]}`)
	if !strings.Contains(output, "error_code: api_key_missing") || !strings.Contains(output, "UNSET_KEY") || len(rig.requests) != 0 {
		t.Fatalf("requests %d:\n%s", len(rig.requests), output)
	}
}

func TestDecideTimeoutStopsASlowProvider(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	settings := sessionfs.ExtensionDecideSettings{Backend: "openai", BaseURL: slow.URL, APIKeyEnv: "K", Model: "m", TimeoutMS: 100, MaxQuestions: 4, MaxInputBytes: 100, MaxResponseBytes: 100}
	tool, err := newDecideTool(settings, DecideImages{}, func(string) string { return "k" }, slow.Client())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	output := executeCatalogCall(t, catalog, "decide", `{"input":"x","questions":[{"instructions":"?"}]}`)
	if !strings.Contains(output, "error_code: timeout") || time.Since(started) > 2*time.Second {
		t.Fatalf("after %v:\n%s", time.Since(started), output)
	}
}

func TestDecideDoesNotFollowARedirectWithTheKey(t *testing.T) {
	var leaked string
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached, leaked = true, r.Header.Get("Authorization")
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	settings := sessionfs.ExtensionDecideSettings{Backend: "openai", BaseURL: redirector.URL, APIKeyEnv: "K", Model: "m", TimeoutMS: 5000, MaxQuestions: 4, MaxInputBytes: 100, MaxResponseBytes: 100}
	// The client the tool builds for itself, with only the key injected.
	tool, err := newDecideTool(settings, DecideImages{}, func(string) string { return "k" }, newDecideHTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), map[string]any{"input": "x", "questions": []any{map[string]any{"instructions": "?"}}}, tools.ExecutionContext{CallID: "c"})
	if err != nil || reached || leaked != "" || !strings.Contains(output, "HTTP 307") {
		t.Fatalf("reached %v, leaked %q, err %v:\n%s", reached, leaked, err, output)
	}
}

func TestDecideCancelledCall(t *testing.T) {
	rig := newDecideRig(t, "openai", 200, openAIReply, nil)
	tool, err := newDecideTool(rig.settings, DecideImages{}, func(string) string { return "k" }, rig.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output, err := tool.Execute(ctx, map[string]any{"input": "x", "questions": []any{map[string]any{"instructions": "?"}}}, tools.ExecutionContext{CallID: "c"})
	if err != nil || !strings.Contains(output, "error_code: cancelled") {
		t.Fatalf("err %v:\n%s", err, output)
	}
}

func TestDecideIsReadOnlyAndShippedDefinitionMatches(t *testing.T) {
	tool, err := NewDecideTool(sessionfs.ExtensionDecideSettings{}, DecideImages{})
	if err != nil {
		t.Fatal(err)
	}
	if !tool.ReadOnly || tool.Definition.ID != DecideToolID || tool.Definition.Name != "decide" {
		t.Fatalf("tool = %+v", tool)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "extension", "decide.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []tools.Definition `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("manifest: %v, %d tools", err, len(document.Tools))
	}
	same, err := sameJSONText(document.Tools[0].InputSchema, DecideInputSchema)
	if err != nil || !same {
		t.Fatalf("sample/tools/extension/decide.json inputSchema differs from the in-code schema (err %v)", err)
	}
	if document.Tools[0].ID != DecideToolID || document.Tools[0].Name != "decide" || document.Tools[0].Description != decideDescription {
		t.Fatalf("manifest definition = %+v", document.Tools[0])
	}
}

// A score outside the levels (a rounding edge, or a provider bug) must still name a
// real level and must not panic on the index.
func TestDecideScoreOutsideTheLevelsIsClamped(t *testing.T) {
	for name, tc := range map[string]struct {
		score float64
		want  string
	}{
		"below zero": {-0.7, "Low"},
		"zero":       {0, "Low"},
		"rounds up":  {0.5, "High"},
		"last":       {1, "High"},
		"above last": {7.9, "High"},
	} {
		t.Run(name, func(t *testing.T) {
			reply, _ := json.Marshal(map[string]any{"answers": map[string]any{"s": map[string]any{"type": "score", "score": tc.score, "confidence": 1, "probabilities": map[string]float64{"0": 0.5, "1": 0.5}}}})
			rig := newDecideRig(t, "openrouter", 200, string(reply), nil)
			output := rig.call(t, `{"input":"x","questions":[{"name":"s","instructions":"?","levels":["Low","High"]}]}`)
			answer := decideContent(t, output)["answers"].([]any)[0].(map[string]any)
			if answer["level"] != tc.want {
				t.Fatalf("score %v -> level %v, want %s\n%s", tc.score, answer["level"], tc.want, output)
			}
		})
	}
}

// An answer that does not fit its question (another type, or a choice without a
// value) must not be shown as a number: a made-up 0 reads as "no".
func TestDecideAnswerThatDoesNotFitItsQuestionIsUnusableNotZero(t *testing.T) {
	const predicate = `{"input":"x","questions":[{"name":"q1","instructions":"?"}]}`
	const choice = `{"input":"x","questions":[{"name":"q1","instructions":"?","choices":["a","b"]}]}`
	for name, tc := range map[string]struct{ backend, reply, arguments, status string }{
		"openrouter choice answer to predicate": {"openrouter", `{"answers":{"q1":{"type":"choice","choice":"a","confidence":1}}}`, predicate, "unusable"},
		"openrouter unknown type":               {"openrouter", `{"answers":{"q1":{"type":"mystery"}}}`, predicate, "unusable"},
		"openrouter null choice":                {"openrouter", `{"answers":{"q1":{"type":"choice","choice":null}}}`, choice, "unusable"},
		"openrouter refusal":                    {"openrouter", `{"answers":{"q1":{"type":"refusal"}}}`, predicate, "refused"},
		"openai choice answer to predicate":     {"openai", `{"answers":[{"type":"choice","name":"q1","choice":"a","confidence":1}]}`, predicate, "unusable"},
		"openai predicate answer to choice":     {"openai", `{"answers":[{"type":"predicate","name":"q1","probability":0.9}]}`, choice, "unusable"},
		"openai null choice":                    {"openai", `{"answers":[{"type":"choice","name":"q1","choice":null}]}`, choice, "unusable"},
		"openai refusal":                        {"openai", `{"answers":[{"type":"refusal","name":"q1"}]}`, predicate, "refused"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideRig(t, tc.backend, 200, tc.reply, nil)
			answer := decideContent(t, rig.call(t, tc.arguments))["answers"].([]any)[0].(map[string]any)
			if answer["status"] != tc.status {
				t.Fatalf("answer = %#v, want status %s", answer, tc.status)
			}
			for _, key := range []string{"probability", "choice", "score", "confidence"} {
				if _, has := answer[key]; has {
					t.Fatalf("an unusable answer carries %s: %#v", key, answer)
				}
			}
		})
	}
}

// A number the provider left out must not become 0, a choice must be one of the
// options, and the key must not come back through a provider's error text.
func TestDecideRejectsMissingNumbersUnknownChoicesAndKeyEcho(t *testing.T) {
	const predicate = `{"input":"x","questions":[{"name":"q1","instructions":"?"}]}`
	const choice = `{"input":"x","questions":[{"name":"q1","instructions":"?","choices":["a","b"]}]}`
	const score = `{"input":"x","questions":[{"name":"q1","instructions":"?","levels":["lo","hi"]}]}`
	for name, tc := range map[string]struct{ backend, reply, arguments string }{
		"openai predicate without probability":    {"openai", `{"answers":[{"type":"predicate","name":"q1"}]}`, predicate},
		"openai predicate null probability":       {"openai", `{"answers":[{"type":"predicate","name":"q1","probability":null}]}`, predicate},
		"openai score without score":              {"openai", `{"answers":[{"type":"score","name":"q1","confidence":1}]}`, score},
		"openrouter predicate without noul":       {"openrouter", `{"answers":{"q1":{"type":"noul"}}}`, predicate},
		"openrouter score without score":          {"openrouter", `{"answers":{"q1":{"type":"score","confidence":1}}}`, score},
		"openai choice not among the options":     {"openai", `{"answers":[{"type":"choice","name":"q1","choice":"A","confidence":1}]}`, choice},
		"openrouter choice not among the options": {"openrouter", `{"answers":{"q1":{"type":"choice","choice":"c","confidence":1}}}`, choice},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideRig(t, tc.backend, 200, tc.reply, nil)
			answer := decideContent(t, rig.call(t, tc.arguments))["answers"].([]any)[0].(map[string]any)
			if answer["status"] != "unusable" {
				t.Fatalf("answer = %#v, want status unusable", answer)
			}
			for _, key := range []string{"probability", "choice", "score", "level", "confidence"} {
				if _, has := answer[key]; has {
					t.Fatalf("an unusable answer carries %s: %#v", key, answer)
				}
			}
		})
	}

	// A real zero still counts as an answer.
	rig := newDecideRig(t, "openai", 200, `{"answers":[{"type":"predicate","name":"q1","probability":0}]}`, nil)
	if answer := decideContent(t, rig.call(t, predicate))["answers"].([]any)[0].(map[string]any); answer["probability"] != 0.0 || answer["status"] != nil {
		t.Fatalf("a real 0 was dropped: %#v", answer)
	}

	// The provider echoes the key in its error text.
	for name, reply := range map[string]string{
		"json message": `{"error":{"message":"Incorrect API key provided: secret-key. Find it at the dashboard."}}`,
		"plain body":   `bad key secret-key`,
		"json string":  `{"error":"secret-key is not valid"}`,
	} {
		t.Run("echoed key, "+name, func(t *testing.T) {
			rig := newDecideRig(t, "openai", 401, reply, nil)
			output := rig.call(t, predicate)
			if strings.Contains(output, "secret-key") || !strings.Contains(output, "[redacted]") {
				t.Fatalf("the key was not redacted:\n%s", output)
			}
		})
	}

	// An entry with no label would write an empty JSON key.
	rig = newDecideRig(t, "openai", 200, `{"answers":[{"type":"choice","name":"q1","choice":"a","confidence":1,"probabilities":[{"value":"a","probability":0.6},{"probability":0.2},{"probability":0.2}]}]}`, nil)
	output := rig.call(t, choice)
	if strings.Contains(output, `"":`) {
		t.Fatalf("an empty probability label was written:\n%s", output)
	}
}

// A transport error can quote the request URL or headers; the key must not survive
// in the reason, and a timeout must still be told apart from other failures.
func TestDecideTransportErrorKeepsItsKindAndHidesTheKey(t *testing.T) {
	err := &decideTransportError{Err: context.DeadlineExceeded, Key: "secret-key"}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the wrapped error lost its kind")
	}
	quoted := &decideTransportError{Err: errors.New(`Post "https://x/?k=secret-key": EOF`), Key: "secret-key"}
	if strings.Contains(quoted.Error(), "secret-key") || !strings.Contains(quoted.Error(), "[redacted]") {
		t.Fatalf("error text = %q", quoted.Error())
	}
	if decideRedact("text", "") != "text" {
		t.Fatal("an empty key must not rewrite the text")
	}
}

// An answer of another type that carries a number would pass a check on the number
// alone: a score answer to a predicate question must not become "probability":0.
func TestDecideAnswerOfAnotherTypeWithANumberIsUnusable(t *testing.T) {
	const predicate = `{"input":"x","questions":[{"name":"q1","instructions":"?"}]}`
	const score = `{"input":"x","questions":[{"name":"q1","instructions":"?","levels":["lo","hi"]}]}`
	for name, tc := range map[string]struct{ backend, reply, arguments string }{
		"openai score answer to predicate":     {"openai", `{"answers":[{"type":"score","name":"q1","score":1,"confidence":1}]}`, predicate},
		"openai predicate answer to score":     {"openai", `{"answers":[{"type":"predicate","name":"q1","probability":0.9}]}`, score},
		"openrouter score answer to predicate": {"openrouter", `{"answers":{"q1":{"type":"score","score":1,"confidence":1}}}`, predicate},
		"openrouter noul answer to score":      {"openrouter", `{"answers":{"q1":{"type":"noul","noul":0.9}}}`, score},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideRig(t, tc.backend, 200, tc.reply, nil)
			answer := decideContent(t, rig.call(t, tc.arguments))["answers"].([]any)[0].(map[string]any)
			if answer["status"] != "unusable" {
				t.Fatalf("answer = %#v, want unusable", answer)
			}
		})
	}
}

// One answer with a field of the wrong kind must cost that answer only.
func TestDecideOneMalformedAnswerDoesNotDiscardTheOthers(t *testing.T) {
	const arguments = `{"input":"x","questions":[{"name":"a","instructions":"?"},{"name":"b","instructions":"?"}]}`
	for name, tc := range map[string]struct{ backend, reply string }{
		"openai":     {"openai", `{"answers":[{"type":"predicate","name":"a","probability":"high"},{"type":"predicate","name":"b","probability":0.7}]}`},
		"openrouter": {"openrouter", `{"answers":{"a":{"type":"noul","noul":"high"},"b":{"type":"noul","noul":0.7}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideRig(t, tc.backend, 200, tc.reply, nil)
			output := rig.call(t, arguments)
			if !strings.Contains(output, `"status":"ok"`) {
				t.Fatalf("one odd field failed the whole call:\n%s", output)
			}
			answers := decideContent(t, output)["answers"].([]any)
			if answers[0].(map[string]any)["status"] != "unusable" || answers[1].(map[string]any)["probability"] != 0.7 {
				t.Fatalf("answers = %#v", answers)
			}
		})
	}
}

const predicateQuestion = `"questions":[{"name":"q1","instructions":"?"}]`

func decideOKReply(backend string) string {
	if backend == "openai" {
		return `{"answers":[{"type":"predicate","name":"q1","probability":0.5}]}`
	}
	return `{"answers":{"q1":{"type":"noul","noul":0.5}}}`
}

func TestDecideInputIsAnArrayOfTextAndJSONParts(t *testing.T) {
	const mixed = `{"input":["Customer complains.",{"type":"text","text":"Second note."},{"type":"json","value":{"charged":20,"expected":10,"tag":"<b>&"}}],` + predicateQuestion + `}`
	t.Run("openrouter sends the parts as a list, JSON as itself", func(t *testing.T) {
		rig := newDecideRig(t, "openrouter", 200, decideOKReply("openrouter"), nil)
		if output := rig.call(t, mixed); !strings.Contains(output, `"status":"ok"`) {
			t.Fatalf("not ok:\n%s", output)
		}
		state, ok := rig.requests[0].Body["state"].([]any)
		if !ok || len(state) != 3 || state[0] != "Customer complains." || state[1] != "Second note." {
			t.Fatalf("state = %#v", rig.requests[0].Body["state"])
		}
		object, ok := state[2].(map[string]any)
		if !ok || object["charged"] != 20.0 || object["tag"] != "<b>&" {
			t.Fatalf("a JSON part must be sent as a JSON value, got %#v", state[2])
		}
	})
	t.Run("openai sends one user message of input_text parts", func(t *testing.T) {
		rig := newDecideRig(t, "openai", 200, decideOKReply("openai"), nil)
		if output := rig.call(t, mixed); !strings.Contains(output, `"status":"ok"`) {
			t.Fatalf("not ok:\n%s", output)
		}
		messages, ok := rig.requests[0].Body["input"].([]any)
		if !ok || len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
			t.Fatalf("input = %#v", rig.requests[0].Body["input"])
		}
		parts := messages[0].(map[string]any)["content"].([]any)
		if len(parts) != 3 {
			t.Fatalf("parts = %#v", parts)
		}
		for index, want := range []string{"Customer complains.", "Second note.", `{"charged":20,"expected":10,"tag":"<b>&"}`} {
			part := parts[index].(map[string]any)
			if part["type"] != "input_text" || part["text"] != want {
				t.Fatalf("part %d = %#v, want input_text %q", index, part, want)
			}
		}
	})
}

func TestDecideAllTextInputIsSentAsOneString(t *testing.T) {
	const input = `{"input":["First.",{"type":"text","text":"Second."}],` + predicateQuestion + `}`
	for backend, field := range map[string]string{"openai": "input", "openrouter": "state"} {
		t.Run(backend, func(t *testing.T) {
			rig := newDecideRig(t, backend, 200, decideOKReply(backend), nil)
			rig.call(t, input)
			if got := rig.requests[0].Body[field]; got != "First.\n\nSecond." {
				t.Fatalf("%s = %#v", field, got)
			}
		})
	}
}

func TestDecideInputAcceptsABareStringOrOnePartButTheSchemaStaysAnArray(t *testing.T) {
	for name, input := range map[string]string{
		"bare string":    `"Just text."`,
		"one text part":  `{"type":"text","text":"Just text."}`,
		"padded type":    `[{"type":" TEXT ","text":"Just text."}]`,
		"plain in array": `["Just text."]`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideRig(t, "openrouter", 200, decideOKReply("openrouter"), nil)
			output := rig.call(t, `{"input":`+input+`,`+predicateQuestion+`}`)
			if !strings.Contains(output, `"status":"ok"`) || rig.requests[0].Body["state"] != "Just text." {
				t.Fatalf("state = %#v\n%s", rig.requests[0].Body["state"], output)
			}
		})
	}
	var schema struct {
		Properties struct {
			Input struct {
				Type     string `json:"type"`
				MinItems int    `json:"minItems"`
			} `json:"input"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(DecideInputSchema, &schema); err != nil || schema.Properties.Input.Type != "array" || schema.Properties.Input.MinItems != 1 {
		t.Fatalf("the model-facing input schema must be an array of at least one part: %+v, %v", schema, err)
	}
}

func TestDecideRefusesInputsItCannotSend(t *testing.T) {
	rig := newDecideRig(t, "openrouter", 200, decideOKReply("openrouter"), nil)
	for name, tc := range map[string]struct{ input, want string }{
		"empty array":        {`[]`, "schema"},
		"empty string":       {`[""]`, "schema"},
		"image part":         {`[{"type":"image","image_url":"data:image/png;base64,AAAA"}]`, "schema"},
		"unknown field":      {`[{"type":"text","text":"x","extra":1}]`, "schema"},
		"text without text":  {`[{"type":"text"}]`, "schema"},
		"json without value": {`[{"type":"json"}]`, "schema"},
		"number":             {`5`, "schema"},
		"blank text":         {`["  \n "]`, "input part 1: the text is blank"},
		"blank text part":    {`["ok",{"type":"text","text":"   "}]`, "input part 2: the text is blank"},
		"null json":          {`[{"type":"json","value":null}]`, "input part 1: the JSON value is null"},
	} {
		t.Run(name, func(t *testing.T) {
			output := rig.call(t, `{"input":`+tc.input+`,`+predicateQuestion+`}`)
			if !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, tc.want) {
				t.Fatalf("want invalid_input containing %q, got:\n%s", tc.want, output)
			}
		})
	}
	if len(rig.requests) != 0 {
		t.Fatalf("%d request(s) sent for refused input", len(rig.requests))
	}
}

func TestDecideAcceptsEveryKindOfJSONValue(t *testing.T) {
	rig := newDecideRig(t, "openrouter", 200, decideOKReply("openrouter"), nil)
	for name, value := range map[string]string{"object": `{"a":1}`, "array": `[1,"two",null]`, "string": `"text"`, "number": `3.5`, "false": `false`, "zero": `0`, "empty object": `{}`} {
		output := rig.call(t, `{"input":[{"type":"json","value":`+value+`}],`+predicateQuestion+`}`)
		if !strings.Contains(output, `"status":"ok"`) {
			t.Errorf("%s value refused:\n%s", name, output)
		}
	}
	if len(rig.requests) != 7 {
		t.Fatalf("%d requests for 7 valid inputs", len(rig.requests))
	}
}

func TestDecideInputLimitCountsAllPartsAndTheJSONAsSent(t *testing.T) {
	rig := newDecideRig(t, "openrouter", 200, decideOKReply("openrouter"), func(s *sessionfs.ExtensionDecideSettings) { s.MaxInputBytes = 20 })
	// 10 text bytes + 10 bytes of compact JSON ({"a":"xxxx"} is 12) stay near the limit.
	if output := rig.call(t, `{"input":["0123456789",{"type":"json","value":{"a":"xx"}}],`+predicateQuestion+`}`); !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("20 bytes refused:\n%s", output)
	}
	output := rig.call(t, `{"input":["0123456789",{"type":"json","value":{"a":"xxxx"}}],`+predicateQuestion+`}`)
	if !strings.Contains(output, "error_code: input_too_large") || !strings.Contains(output, "Input is 22 bytes; the limit is 20") {
		t.Fatalf("got:\n%s", output)
	}
	if len(rig.requests) != 1 {
		t.Fatalf("%d requests; the oversized input must send none", len(rig.requests))
	}
	// Whitespace inside a JSON value is not counted: it is sent compact.
	output = rig.call(t, "{\"input\":[{\"type\":\"json\",\"value\":{\"a\" :   \"xx\"   }}],"+predicateQuestion+"}")
	if !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("got:\n%s", output)
	}
}

func sameJSONText(left, right json.RawMessage) (bool, error) {
	var l, r any
	if err := json.Unmarshal(left, &l); err != nil {
		return false, err
	}
	if err := json.Unmarshal(right, &r); err != nil {
		return false, err
	}
	return reflect.DeepEqual(l, r), nil
}

func executeCatalogCall(t *testing.T, catalog *tools.Catalog, name, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_1", "name": name, "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item.Output
}
