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
	"strconv"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// decideSystemoneRig is a decide_systemone tool wired to a fake provider. Requests records each request
// the provider received.
type decideSystemoneRig struct {
	catalog  *tools.Catalog
	settings sessionfs.ExtensionDecideSystemoneSettings
	requests []decideSystemoneRecorded
	server   *httptest.Server
}

type decideSystemoneRecorded struct {
	Method, Path, Authorization string
	Body                        map[string]any
	Raw                         []byte
}

func newDecideSystemoneRig(t *testing.T, status int, reply string, adjust func(*sessionfs.ExtensionDecideSystemoneSettings)) *decideSystemoneRig {
	t.Helper()
	rig := &decideSystemoneRig{}
	rig.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		recorded := decideSystemoneRecorded{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Raw: data}
		_ = json.Unmarshal(data, &recorded.Body)
		rig.requests = append(rig.requests, recorded)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(rig.server.Close)
	rig.settings = sessionfs.ExtensionDecideSystemoneSettings{BaseURL: rig.server.URL, Path: "/decisions", APIKeyEnv: "DECIDE_KEY", Model: "m-1", TimeoutMS: 5000, MaxQuestions: 4, MaxInputBytes: 1000, MaxResponseBytes: 4096}
	if adjust != nil {
		adjust(&rig.settings)
	}
	environment := func(name string) string {
		if name == "DECIDE_KEY" {
			return " secret-key "
		}
		return ""
	}
	tool, err := newDecideSystemoneTool(rig.settings, environment, rig.server.Client())
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

func (r *decideSystemoneRig) call(t *testing.T, arguments string) string {
	t.Helper()
	return executeCatalogCall(t, r.catalog, "decide_systemone", arguments)
}

// content returns the JSON body of an ok result.
func decideSystemoneContent(t *testing.T, output string) map[string]any {
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

func TestDecideSystemoneRequestAndAnswers(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, openRouterReply, nil)
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

	answers := decideSystemoneContent(t, output)["answers"].([]any)
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

func TestDecideSystemoneMarksAQuestionTheProviderDidNotAnswer(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{"a":{"type":"noul","noul":0}}}`, nil)
	output := rig.call(t, `{"input":"x","questions":[{"name":"a","instructions":"A?"},{"name":"b","instructions":"B?"}]}`)
	answers := decideSystemoneContent(t, output)["answers"].([]any)
	if answers[1].(map[string]any)["status"] != "missing" {
		t.Fatalf("a gap must be reported, not shown as a negative: %#v", answers[1])
	}
	if answers[0].(map[string]any)["probability"] != 0.0 {
		t.Fatalf("a real zero must stay a zero: %#v", answers[0])
	}
}

func TestDecideSystemoneSmallestCallAndNormalizedArguments(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{"q1":{"type":"noul","noul":1},"q2":{"type":"noul","noul":0}}}`, nil)
	// No name, no type; "NOUL" and padding are repaired instead of rejected.
	output := rig.call(t, `{"input":"x","questions":[{"instructions":"  Is it x?  "},{"type":" NOUL ","instructions":"Is it y?"}]}`)
	if !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("not ok:\n%s", output)
	}
	questions := rig.requests[0].Body["questions"].(map[string]any)
	if questions["q1"].(map[string]any)["instructions"] != "Is it x?" || questions["q2"].(map[string]any)["type"] != "noul" {
		t.Fatalf("questions sent = %#v", questions)
	}
	answers := decideSystemoneContent(t, output)["answers"].([]any)
	if answers[0].(map[string]any)["name"] != "q1" || answers[1].(map[string]any)["type"] != "predicate" {
		t.Fatalf("answers = %#v", answers)
	}
}

func TestDecideSystemoneRefusesBadQuestionsBeforeAnyRequest(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{}}`, nil)
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
		"input too large":         {`{"input":"` + strings.Repeat("x", 1001) + `","questions":[{"instructions":"?"}]}`, `The request is 1042 bytes (1001 input, 41 questions); max_input_bytes is 1000`},
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

func TestDecideSystemoneSchemaRejectsMissingParts(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{}}`, nil)
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

func TestDecideSystemoneReportsProviderFailures(t *testing.T) {
	const call = `{"input":"x","questions":[{"instructions":"?"}]}`
	for name, tc := range map[string]struct {
		reply       string
		status      int
		code, state string
	}{
		"unauthorized":     {`{"error":{"message":"Missing Authentication header","code":401}}`, 401, "unauthorized", "error"},
		"rejected request": {`{"error":{"message":"[{\"code\":\"invalid_union\"}]","code":400}}`, 400, "request_rejected", "invalid_input"},
		"not json":         {`<html>`, 200, "bad_response", "error"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideSystemoneRig(t, tc.status, tc.reply, nil)
			output := rig.call(t, call)
			if !strings.Contains(output, `"status":"`+tc.state+`"`) || !strings.Contains(output, `error_code: `+tc.code) {
				t.Fatalf("want %s/%s, got:\n%s", tc.state, tc.code, output)
			}
		})
	}
}

func TestDecideSystemoneErrorTextIsOneBoundedLineAndNeverHoldsTheKey(t *testing.T) {
	long := `{"error":{"message":"` + strings.Repeat("line one\\n", 200) + `"}}`
	rig := newDecideSystemoneRig(t, 400, long, nil)
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

// A provider that echoes the key into a long message puts it across the
// 400-byte cut. Redacting after the cut left a prefix of the key in the
// reported text, so the redaction has to run first.
func TestDecideSystemoneErrorTextRedactsAKeyAcrossTheCut(t *testing.T) {
	const key = "secret-key"
	// Put the key across byte 400: the text before "key=" fills 396 bytes, so a
	// cut made before the redaction keeps the first four characters of the key.
	straddle := func(before int) string {
		return strings.Repeat("x", 400-before-len("key=")-4)
	}
	for name, reply := range map[string]string{
		"message": `{"error":{"message":"` + straddle(0) + `key=` + key + `"}}`,
		"detail":  `{"detail":[{"loc":["body"],"msg":"` + straddle(len("body: ")) + `key=` + key + `"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 400, reply, nil)
			output := rig.call(t, `{"input":"x","questions":[{"instructions":"?"}]}`)
			if !strings.Contains(output, "error_code: request_rejected") {
				t.Fatalf("want a rejected request, got:\n%s", output)
			}
			for prefix := 4; prefix <= len(key); prefix++ {
				if strings.Contains(output, key[:prefix]) {
					t.Fatalf("%d of %d key characters survived the cut:\n%s", prefix, len(key), output)
				}
			}
			if !strings.Contains(output, strings.Repeat("x", 300)) {
				t.Fatalf("the provider text is gone:\n%s", output)
			}
		})
	}
}

func TestDecideSystemoneRefusesAnOversizedResponse(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{},"pad":"`+strings.Repeat("x", 5000)+`"}`, nil)
	output := rig.call(t, `{"input":"x","questions":[{"instructions":"?"}]}`)
	if !strings.Contains(output, "error_code: bad_response") || !strings.Contains(output, "larger than 4096 bytes") {
		t.Fatalf("got:\n%s", output)
	}
}

func TestDecideSystemoneMissingKeyDoesNotCallTheProvider(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, openRouterReply, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.APIKeyEnv = "UNSET_KEY" })
	output := rig.call(t, `{"input":"x","questions":[{"instructions":"?"}]}`)
	if !strings.Contains(output, "error_code: api_key_missing") || !strings.Contains(output, "UNSET_KEY") || len(rig.requests) != 0 {
		t.Fatalf("requests %d:\n%s", len(rig.requests), output)
	}
}

func TestDecideSystemoneTimeoutStopsASlowProvider(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	settings := sessionfs.ExtensionDecideSystemoneSettings{BaseURL: slow.URL, Path: "/decisions", APIKeyEnv: "K", Model: "m", TimeoutMS: 100, MaxQuestions: 4, MaxInputBytes: 100, MaxResponseBytes: 100}
	tool, err := newDecideSystemoneTool(settings, func(string) string { return "k" }, slow.Client())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	output := executeCatalogCall(t, catalog, "decide_systemone", `{"input":"x","questions":[{"instructions":"?"}]}`)
	if !strings.Contains(output, "error_code: timeout") || time.Since(started) > 2*time.Second {
		t.Fatalf("after %v:\n%s", time.Since(started), output)
	}
}

func TestDecideSystemoneDoesNotFollowARedirectWithTheKey(t *testing.T) {
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
	settings := sessionfs.ExtensionDecideSystemoneSettings{BaseURL: redirector.URL, Path: "/decisions", APIKeyEnv: "K", Model: "m", TimeoutMS: 5000, MaxQuestions: 4, MaxInputBytes: 100, MaxResponseBytes: 100}
	// The client the tool builds for itself, with only the key injected.
	tool, err := newDecideSystemoneTool(settings, func(string) string { return "k" }, newDecideSystemoneHTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), map[string]any{"input": "x", "questions": []any{map[string]any{"instructions": "?"}}}, tools.ExecutionContext{CallID: "c"})
	if err != nil || reached || leaked != "" || !strings.Contains(output, "HTTP 307") {
		t.Fatalf("reached %v, leaked %q, err %v:\n%s", reached, leaked, err, output)
	}
}

func TestDecideSystemoneCancelledCall(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, openRouterReply, nil)
	tool, err := newDecideSystemoneTool(rig.settings, func(string) string { return "k" }, rig.server.Client())
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

func TestDecideSystemoneIsReadOnlyAndShippedDefinitionMatches(t *testing.T) {
	tool, err := NewDecideSystemoneTool(sessionfs.ExtensionDecideSystemoneSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if !tool.ReadOnly || tool.Definition.ID != DecideSystemoneToolID || tool.Definition.Name != "decide_systemone" {
		t.Fatalf("tool = %+v", tool)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "extension", "decide_systemone.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []tools.Definition `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("manifest: %v, %d tools", err, len(document.Tools))
	}
	same, err := sameJSONText(document.Tools[0].InputSchema, DecideSystemoneInputSchema)
	if err != nil || !same {
		t.Fatalf("sample/tools/extension/decide_systemone.json inputSchema differs from the in-code schema (err %v)", err)
	}
	if document.Tools[0].ID != DecideSystemoneToolID || document.Tools[0].Name != "decide_systemone" || document.Tools[0].Description != decideSystemoneDescription {
		t.Fatalf("manifest definition = %+v", document.Tools[0])
	}
}

// A score outside the levels (a rounding edge, or a provider bug) must still name a
// real level and must not panic on the index.
func TestDecideSystemoneScoreOutsideTheLevelsIsClamped(t *testing.T) {
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
			rig := newDecideSystemoneRig(t, 200, string(reply), nil)
			output := rig.call(t, `{"input":"x","questions":[{"name":"s","instructions":"?","levels":["Low","High"]}]}`)
			answer := decideSystemoneContent(t, output)["answers"].([]any)[0].(map[string]any)
			if answer["level"] != tc.want {
				t.Fatalf("score %v -> level %v, want %s\n%s", tc.score, answer["level"], tc.want, output)
			}
		})
	}
}

// An answer that does not fit its question (another type, or a choice without a
// value) must not be shown as a number: a made-up 0 reads as "no".
func TestDecideSystemoneAnswerThatDoesNotFitItsQuestionIsUnusableNotZero(t *testing.T) {
	const predicate = `{"input":"x","questions":[{"name":"q1","instructions":"?"}]}`
	const choice = `{"input":"x","questions":[{"name":"q1","instructions":"?","choices":["a","b"]}]}`
	for name, tc := range map[string]struct{ reply, arguments, status string }{
		"openrouter choice answer to predicate": {`{"answers":{"q1":{"type":"choice","choice":"a","confidence":1}}}`, predicate, "unusable"},
		"openrouter unknown type":               {`{"answers":{"q1":{"type":"mystery"}}}`, predicate, "unusable"},
		"openrouter null choice":                {`{"answers":{"q1":{"type":"choice","choice":null}}}`, choice, "unusable"},
		"openrouter refusal":                    {`{"answers":{"q1":{"type":"refusal"}}}`, predicate, "refused"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 200, tc.reply, nil)
			answer := decideSystemoneContent(t, rig.call(t, tc.arguments))["answers"].([]any)[0].(map[string]any)
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
func TestDecideSystemoneRejectsMissingNumbersUnknownChoicesAndKeyEcho(t *testing.T) {
	const predicate = `{"input":"x","questions":[{"name":"q1","instructions":"?"}]}`
	const choice = `{"input":"x","questions":[{"name":"q1","instructions":"?","choices":["a","b"]}]}`
	const score = `{"input":"x","questions":[{"name":"q1","instructions":"?","levels":["lo","hi"]}]}`
	for name, tc := range map[string]struct{ reply, arguments string }{
		"openrouter predicate without noul":       {`{"answers":{"q1":{"type":"noul"}}}`, predicate},
		"openrouter score without score":          {`{"answers":{"q1":{"type":"score","confidence":1}}}`, score},
		"openrouter choice not among the options": {`{"answers":{"q1":{"type":"choice","choice":"c","confidence":1}}}`, choice},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 200, tc.reply, nil)
			answer := decideSystemoneContent(t, rig.call(t, tc.arguments))["answers"].([]any)[0].(map[string]any)
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
	rig := newDecideSystemoneRig(t, 200, `{"answers":{"q1":{"type":"noul","noul":0}}}`, nil)
	if answer := decideSystemoneContent(t, rig.call(t, predicate))["answers"].([]any)[0].(map[string]any); answer["probability"] != 0.0 || answer["status"] != nil {
		t.Fatalf("a real 0 was dropped: %#v", answer)
	}

	// The provider echoes the key in its error text.
	for name, reply := range map[string]string{
		"json message": `{"error":{"message":"Incorrect API key provided: secret-key. Find it at the dashboard."}}`,
		"plain body":   `bad key secret-key`,
		"json string":  `{"error":"secret-key is not valid"}`,
		"422 detail":   `{"detail":[{"loc":["body","state"],"msg":"bad secret-key","type":"missing"}]}`,
	} {
		t.Run("echoed key, "+name, func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 401, reply, nil)
			output := rig.call(t, predicate)
			if strings.Contains(output, "secret-key") || !strings.Contains(output, "[redacted]") {
				t.Fatalf("the key was not redacted:\n%s", output)
			}
		})
	}
}

// A transport error can quote the request URL or headers; the key must not survive
// in the reason, and a timeout must still be told apart from other failures.
func TestDecideSystemoneTransportErrorKeepsItsKindAndHidesTheKey(t *testing.T) {
	err := &decideSystemoneTransportError{Err: context.DeadlineExceeded, Key: "secret-key"}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the wrapped error lost its kind")
	}
	quoted := &decideSystemoneTransportError{Err: errors.New(`Post "https://x/?k=secret-key": EOF`), Key: "secret-key"}
	if strings.Contains(quoted.Error(), "secret-key") || !strings.Contains(quoted.Error(), "[redacted]") {
		t.Fatalf("error text = %q", quoted.Error())
	}
	if decideSystemoneRedact("text", "") != "text" {
		t.Fatal("an empty key must not rewrite the text")
	}
}

// An answer of another type that carries a number would pass a check on the number
// alone: a score answer to a predicate question must not become "probability":0.
func TestDecideSystemoneAnswerOfAnotherTypeWithANumberIsUnusable(t *testing.T) {
	const predicate = `{"input":"x","questions":[{"name":"q1","instructions":"?"}]}`
	const score = `{"input":"x","questions":[{"name":"q1","instructions":"?","levels":["lo","hi"]}]}`
	for name, tc := range map[string]struct{ reply, arguments string }{
		"openrouter score answer to predicate": {`{"answers":{"q1":{"type":"score","score":1,"confidence":1}}}`, predicate},
		"openrouter noul answer to score":      {`{"answers":{"q1":{"type":"noul","noul":0.9}}}`, score},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 200, tc.reply, nil)
			answer := decideSystemoneContent(t, rig.call(t, tc.arguments))["answers"].([]any)[0].(map[string]any)
			if answer["status"] != "unusable" {
				t.Fatalf("answer = %#v, want unusable", answer)
			}
		})
	}
}

// One answer with a field of the wrong kind must cost that answer only.
func TestDecideSystemoneOneMalformedAnswerDoesNotDiscardTheOthers(t *testing.T) {
	const arguments = `{"input":"x","questions":[{"name":"a","instructions":"?"},{"name":"b","instructions":"?"}]}`
	for name, tc := range map[string]struct{ reply string }{
		"openrouter": {`{"answers":{"a":{"type":"noul","noul":"high"},"b":{"type":"noul","noul":0.7}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 200, tc.reply, nil)
			output := rig.call(t, arguments)
			if !strings.Contains(output, `"status":"ok"`) {
				t.Fatalf("one odd field failed the whole call:\n%s", output)
			}
			answers := decideSystemoneContent(t, output)["answers"].([]any)
			if answers[0].(map[string]any)["status"] != "unusable" || answers[1].(map[string]any)["probability"] != 0.7 {
				t.Fatalf("answers = %#v", answers)
			}
		})
	}
}

const predicateQuestion = `"questions":[{"name":"q1","instructions":"?"}]`

func decideSystemoneOKReply() string {
	return `{"answers":{"q1":{"type":"noul","noul":0.5}}}`
}

func TestDecideSystemoneInputIsAnArrayOfTextAndJSONParts(t *testing.T) {
	const mixed = `{"input":["Customer complains.",{"type":"text","text":"Second note."},{"type":"json","value":{"charged":20,"expected":10,"tag":"<b>&"}}],` + predicateQuestion + `}`
	t.Run("openrouter sends the parts as a list, JSON as itself", func(t *testing.T) {
		rig := newDecideSystemoneRig(t, 200, decideSystemoneOKReply(), nil)
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
}

func TestDecideSystemoneAllTextInputIsSentAsOneString(t *testing.T) {
	const input = `{"input":["First.",{"type":"text","text":"Second."}],` + predicateQuestion + `}`
	rig := newDecideSystemoneRig(t, 200, decideSystemoneOKReply(), nil)
	rig.call(t, input)
	if got := rig.requests[0].Body["state"]; got != "First.\n\nSecond." {
		t.Fatalf("state = %#v", got)
	}
}

func TestDecideSystemoneInputAcceptsABareStringOrOnePartButTheSchemaStaysAnArray(t *testing.T) {
	for name, input := range map[string]string{
		"bare string":    `"Just text."`,
		"one text part":  `{"type":"text","text":"Just text."}`,
		"padded type":    `[{"type":" TEXT ","text":"Just text."}]`,
		"plain in array": `["Just text."]`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 200, decideSystemoneOKReply(), nil)
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
	if err := json.Unmarshal(DecideSystemoneInputSchema, &schema); err != nil || schema.Properties.Input.Type != "array" || schema.Properties.Input.MinItems != 1 {
		t.Fatalf("the model-facing input schema must be an array of at least one part: %+v, %v", schema, err)
	}
}

func TestDecideSystemoneRefusesInputsItCannotSend(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, decideSystemoneOKReply(), nil)
	for name, tc := range map[string]struct{ input, want string }{
		"empty array":        {`[]`, "schema"},
		"empty string":       {`[""]`, "schema"},
		"image part":         {`[{"type":"image","image_url":"data:image/png;base64,AAAA"}]`, "schema"},
		"image path part":    {`[{"type":"image","path":"a.png"}]`, "schema"},
		"image data part":    {`[{"type":"image","data":"AAAA"}]`, "schema"},
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

func TestDecideSystemoneAcceptsEveryKindOfJSONValue(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, decideSystemoneOKReply(), nil)
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

// questionBytes is what predicateQuestion is sent as: {"q1":{"instructions":"?","type":"noul"}}.
const questionBytes = 41

func TestDecideSystemoneInputLimitCountsAllPartsAndTheJSONAsSent(t *testing.T) {
	// max_input_bytes covers the input parts and the questions together.
	rig := newDecideSystemoneRig(t, 200, decideSystemoneOKReply(), func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.MaxInputBytes = 20 + questionBytes })
	// 10 text bytes + 10 bytes of compact JSON ({"a":"xxxx"} is 12) stay near the limit.
	if output := rig.call(t, `{"input":["0123456789",{"type":"json","value":{"a":"xx"}}],`+predicateQuestion+`}`); !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("input at the limit refused:\n%s", output)
	}
	output := rig.call(t, `{"input":["0123456789",{"type":"json","value":{"a":"xxxx"}}],`+predicateQuestion+`}`)
	if !strings.Contains(output, "error_code: input_too_large") || !strings.Contains(output, "The request is 63 bytes (22 input, 41 questions); max_input_bytes is 61") {
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

func TestDecideSystemoneUsesTheConfiguredPathForTheTypeSafeAPI(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"model":"jev-latest","answers":{"q1":{"type":"noul","noul":0.5}},"usage":{"input_tokens":1,"output_tokens":1}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) {
		s.Path, s.Model = "/v1/systemone", "jev-latest"
	})
	output := rig.call(t, `{"input":"x",`+predicateQuestion+`}`)
	request := rig.requests[0]
	if !strings.Contains(output, `"status":"ok"`) || request.Path != "/v1/systemone" || request.Authorization != "Bearer secret-key" || request.Body["model"] != "jev-latest" {
		t.Fatalf("request = %+v\n%s", request, output)
	}
}

func TestDecideSystemoneReportsA422DetailWithItsLocation(t *testing.T) {
	reply := `{"detail":[{"loc":["body","questions","q1","criteria"],"msg":"Field required","type":"missing"},{"loc":["body","state"],"msg":"Input should be a valid string","type":"string_type"}]}`
	rig := newDecideSystemoneRig(t, 422, reply, nil)
	output := rig.call(t, `{"input":"x",`+predicateQuestion+`}`)
	if !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, "error_code: request_rejected") ||
		!strings.Contains(output, "body.questions.q1.criteria: Field required; body.state: Input should be a valid string") {
		t.Fatalf("got:\n%s", output)
	}
	var items []string
	for i := 0; i < 30; i++ {
		items = append(items, `{"loc":["body","questions","q`+strings.Repeat("x", 20)+`"],"msg":"line one\nline two","type":"x"}`)
	}
	rig = newDecideSystemoneRig(t, 422, `{"detail":[`+strings.Join(items, ",")+`]}`, nil)
	output = rig.call(t, `{"input":"x",`+predicateQuestion+`}`)
	bounded := false
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "reason: ") {
			bounded = len(line) <= 420 && strings.HasSuffix(line, "...")
		}
	}
	if !bounded {
		t.Fatalf("reason not bounded to one line:\n%s", output)
	}
	rig = newDecideSystemoneRig(t, 422, `{"detail":"plain detail"}`, nil)
	if output = rig.call(t, `{"input":"x",`+predicateQuestion+`}`); !strings.Contains(output, "plain detail") {
		t.Fatalf("got:\n%s", output)
	}
}

// max_input_bytes counts the JSON input parts as they are encoded. json.Marshal used to
// HTML-escape the same bytes again on the wire, so a payload of <, > and & counted smaller
// than it was sent; the request now goes out through the shared marshaler.
func TestDecideSystemoneRequestKeepsTheJSONInputBytesItCounted(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{"q":{"type":"noul","noul":0.9}}}`, func(settings *sessionfs.ExtensionDecideSystemoneSettings) {
		settings.MaxInputBytes = 20000
	})
	rig.call(t, `{"input":[{"type":"json","value":{"x":"a<b&c","y":"\u2028"}}],"questions":[{"name":"q","instructions":"Is it fine?"}]}`)
	if len(rig.requests) != 1 {
		t.Fatalf("%d requests", len(rig.requests))
	}
	raw := string(rig.requests[0].Raw)
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`, `\u2028`} {
		if strings.Contains(raw, escaped) {
			t.Fatalf("request body contains %s: %s", escaped, raw)
		}
	}
	if !strings.Contains(raw, `"a<b&c"`) {
		t.Fatalf("request body = %s, want the JSON input as it was counted", raw)
	}
	if !strings.Contains(raw, "\u2028") {
		t.Fatalf("request body = %s, want the raw U+2028 separator", raw)
	}
}

// A confidence the provider did not send must stay missing instead of being shown as
// a 0, and a probability outside 0-1 is a provider bug rather than an answer.
func TestDecideSystemoneKeepsAMissingConfidenceMissingAndRefusesAnOutOfRangeOne(t *testing.T) {
	const predicate = `{"input":"x","questions":[{"name":"q1","instructions":"?"}]}`
	const choice = `{"input":"x","questions":[{"name":"q1","instructions":"?","choices":["a","b"]}]}`
	const score = `{"input":"x","questions":[{"name":"q1","instructions":"?","levels":["lo","hi"]}]}`
	for name, tc := range map[string]struct {
		reply, arguments string
		wantStatus       bool // true: no status key, the answer is shown
		wantConfidence   any  // nil: the answer carries no confidence key
		wantNumber       any  // probability or score, when the answer is shown
	}{
		"choice without confidence":            {`{"answers":{"q1":{"type":"choice","choice":"a"}}}`, choice, true, nil, nil},
		"choice with confidence 0":             {`{"answers":{"q1":{"type":"choice","choice":"a","confidence":0}}}`, choice, true, 0.0, nil},
		"choice with confidence 1":             {`{"answers":{"q1":{"type":"choice","choice":"a","confidence":1}}}`, choice, true, 1.0, nil},
		"choice with confidence above one":     {`{"answers":{"q1":{"type":"choice","choice":"a","confidence":1.5}}}`, choice, false, nil, nil},
		"choice with negative confidence":      {`{"answers":{"q1":{"type":"choice","choice":"a","confidence":-0.2}}}`, choice, false, nil, nil},
		"score with confidence above one":      {`{"answers":{"q1":{"type":"score","score":0.5,"confidence":1.5}}}`, score, false, nil, nil},
		"score with confidence 0":              {`{"answers":{"q1":{"type":"score","score":0.5,"confidence":0}}}`, score, true, 0.0, 0.5},
		"predicate with probability 0":         {`{"answers":{"q1":{"type":"noul","noul":0}}}`, predicate, true, nil, 0.0},
		"predicate with probability 1":         {`{"answers":{"q1":{"type":"noul","noul":1}}}`, predicate, true, nil, 1.0},
		"predicate with probability above one": {`{"answers":{"q1":{"type":"noul","noul":1.5}}}`, predicate, false, nil, nil},
		"predicate with negative probability":  {`{"answers":{"q1":{"type":"noul","noul":-0.2}}}`, predicate, false, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 200, tc.reply, nil)
			answer := decideSystemoneContent(t, rig.call(t, tc.arguments))["answers"].([]any)[0].(map[string]any)
			if !tc.wantStatus {
				if answer["status"] != "unusable" {
					t.Fatalf("answer = %#v, want status unusable", answer)
				}
				for _, key := range []string{"probability", "choice", "score", "level", "confidence"} {
					if _, has := answer[key]; has {
						t.Fatalf("an unusable answer carries %s: %#v", key, answer)
					}
				}
				return
			}
			if _, has := answer["status"]; has {
				t.Fatalf("answer = %#v, want it shown", answer)
			}
			confidence, has := answer["confidence"]
			if tc.wantConfidence == nil && has {
				t.Fatalf("a missing confidence was invented: %#v", answer)
			}
			if tc.wantConfidence != nil && confidence != tc.wantConfidence {
				t.Fatalf("confidence = %v, want %v (%#v)", confidence, tc.wantConfidence, answer)
			}
			if tc.wantNumber != nil {
				number := answer["probability"]
				if _, isScore := answer["score"]; isScore {
					number = answer["score"]
				}
				if number != tc.wantNumber {
					t.Fatalf("number = %v, want %v (%#v)", number, tc.wantNumber, answer)
				}
			}
		})
	}
}

// The questions are sent as well: a long instruction or a long option used to be
// measured by nothing and went out as one unbounded request. The input parts and the
// questions share max_input_bytes, so an oversized question costs no request and the
// error names the setting.
func TestDecideSystemoneRefusesQuestionsPastTheByteBudget(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, decideSystemoneOKReply(), nil)
	// The rig's limit is 1000 bytes.
	longInstruction := `{"input":"x","questions":[{"name":"q1","instructions":"` + strings.Repeat("i", 2000) + `"}]}`
	output := rig.call(t, longInstruction)
	if !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, "error_code: input_too_large") {
		t.Fatalf("a long instruction was not refused:\n%s", output)
	}
	if !strings.Contains(output, "max_input_bytes is 1000") {
		t.Fatalf("the error does not name the setting:\n%s", output)
	}
	// The option count is bounded by the same budget, which is why there is no
	// max_options: 40 long options cannot fit either.
	options := make([]string, 0, 40)
	for index := range 40 {
		options = append(options, `"option `+strconv.Itoa(index)+` `+strings.Repeat("o", 40)+`"`)
	}
	longOptions := `{"input":"x","questions":[{"name":"q1","instructions":"?","choices":[` + strings.Join(options, ",") + `]}]}`
	if output := rig.call(t, longOptions); !strings.Contains(output, "error_code: input_too_large") {
		t.Fatalf("40 long options were not refused:\n%s", output)
	}
	if len(rig.requests) != 0 {
		t.Fatalf("%d requests; an oversized question must send none", len(rig.requests))
	}
	// A question that fits still goes out, and the budget counts what is sent.
	output = rig.call(t, `{"input":"x","questions":[{"name":"q1","instructions":"`+strings.Repeat("i", 400)+`"}]}`)
	if !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("a question that fits was refused:\n%s", output)
	}
	if len(rig.requests) != 1 || !strings.Contains(string(rig.requests[0].Raw), strings.Repeat("i", 400)) {
		t.Fatalf("the fitting question did not reach the provider: %#v", rig.requests)
	}
}

// A question name is data, not a Go literal: the item's keys are written from a fixed
// list and every value, the name included, is encoded as JSON, so a control character in
// a name round-trips instead of rendering a result that is not JSON (p6a C1 / P6AM-20).
func TestDecideSystemoneQuestionNameWithAControlCharacterRoundTrips(t *testing.T) {
	const name = "a\u0001b\u007f"
	reply, _ := json.Marshal(map[string]any{"answers": map[string]any{name: map[string]any{"type": "noul", "noul": 0.5}}})
	rig := newDecideSystemoneRig(t, 200, string(reply), nil)
	arguments, _ := json.Marshal(map[string]any{"input": "x", "questions": []any{
		map[string]any{"name": name, "instructions": "?"},
	}})
	output := rig.call(t, string(arguments))
	answer := decideSystemoneContent(t, output)["answers"].([]any)[0].(map[string]any)
	if got := answer["name"]; got != name {
		t.Fatalf("name = %q, want %q\n%s", got, name, output)
	}
	if answer["probability"] != 0.5 {
		t.Fatalf("probability = %v, want 0.5\n%s", answer["probability"], output)
	}
	// A raw U+0001 would make the result invalid JSON (decideSystemoneContent above
	// already parses it); U+007F is legal unescaped, so only the former is asserted.
	if strings.ContainsRune(output, '\u0001') {
		t.Fatalf("a raw control character reached the result: %q", output)
	}
}
