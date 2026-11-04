package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// schemaProvider is a fake Responses endpoint that answers with answer and keeps the
// text field of every request it receives.
type schemaProvider struct {
	mu     sync.Mutex
	answer string
	texts  []json.RawMessage
}

func (p *schemaProvider) lastText() json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.texts) == 0 {
		return nil
	}
	return p.texts[len(p.texts)-1]
}

func useSchemaProviderFixture(t *testing.T, provider *schemaProvider) layouttest.Fixture {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Text json.RawMessage `json:"text"`
		}
		_ = json.Unmarshal(raw, &request)
		provider.mu.Lock()
		provider.texts = append(provider.texts, request.Text)
		answer, _ := json.Marshal(provider.answer)
		provider.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%s}]}}\n\n", answer)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7},\"output\":[]}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	fixture := useFixture(t, layouttest.Spec{
		Models: map[string]map[string]any{"openai/default": {"provider": "openai", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": server.URL + "/v1"}},
		Server: map[string]any{"log_level": "off"},
	})
	t.Setenv("FIXTURE_KEY", "k")
	return fixture
}

const testOutputSchema = `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`

func writeOutputSchema(t *testing.T, schema string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(schema), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunOutputSchemaSendsTheFormatAndPrintsMatchingJSON(t *testing.T) {
	provider := &schemaProvider{answer: "```json\n{ \"answer\": 42 }\n```"}
	fixture := useSchemaProviderFixture(t, provider)
	session := "local:" + fixture.SessionID
	schemaPath := writeOutputSchema(t, testOutputSchema)

	stdout, stderr, err := runJad(t, "", "run", "--session", session, "--output-schema", schemaPath, "what is six times seven")
	if err != nil || stdout != "{\"answer\":42}\n" {
		t.Fatalf("text: stdout %q stderr %q err %v", stdout, stderr, err)
	}
	want := `{"format":{"type":"json_schema","name":"output","schema":` + testOutputSchema + `,"strict":true}}`
	if got := string(provider.lastText()); got != want {
		t.Fatalf("provider text = %s\nwant %s", got, want)
	}

	stdout, _, err = runJad(t, "", "run", "--session", session, "--format", "json", "--output-schema", schemaPath, "--output-strict=false", "again")
	var result headless.Result
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.Status != headless.StatusCompleted || string(result.Output) != `{"answer":42}` {
		t.Fatalf("json: %q %v", stdout, err)
	}
	if !strings.Contains(string(provider.lastText()), `"strict":false`) {
		t.Fatalf("--output-strict=false not sent: %s", provider.lastText())
	}

	// The format rides on the request only: the transcript holds no trace of it.
	transcript := filepath.Join(fixture.SessionStateDir, "dev", "coder", "messages.jsonl")
	data, err := os.ReadFile(transcript)
	if err != nil || strings.Contains(string(data), "json_schema") || strings.Contains(string(data), "additionalProperties") {
		t.Fatalf("transcript: %v %s", err, data)
	}

	// Without the flag nothing is sent.
	if _, _, err := runJad(t, "", "run", "--session", session, "plain"); err != nil {
		t.Fatal(err)
	}
	if text := provider.lastText(); len(text) != 0 {
		t.Fatalf("a run without --output-schema sent text %s", text)
	}
}

func TestRunOutputSchemaRefusesAnAnswerThatDoesNotMatch(t *testing.T) {
	provider := &schemaProvider{}
	fixture := useSchemaProviderFixture(t, provider)
	session := "local:" + fixture.SessionID
	schemaPath := writeOutputSchema(t, testOutputSchema)

	for name, test := range map[string]struct{ answer, reason string }{
		"not json":       {"forty-two", "is not JSON"},
		"wrong type":     {`{"answer":"42"}`, "/answer"},
		"missing member": {`{}`, "does not match the output schema"},
	} {
		provider.mu.Lock()
		provider.answer = test.answer
		provider.mu.Unlock()
		stdout, stderr, err := runJad(t, "", "run", "--session", session, "--output-schema", schemaPath, "hi")
		if exitCode(err) != exitFailed || stdout != "" || !strings.Contains(stderr, "run invalid_output") || !strings.Contains(stderr, test.reason) {
			t.Errorf("%s: exit %d stdout %q stderr %q", name, exitCode(err), stdout, stderr)
		}
		stdout, _, err = runJad(t, "", "run", "--session", session, "--format", "json", "--output-schema", schemaPath, "hi")
		var result headless.Result
		if exitCode(err) != exitFailed || json.Unmarshal([]byte(stdout), &result) != nil || result.Status != headless.StatusInvalidOutput || result.Text != test.answer || len(result.Output) != 0 {
			t.Errorf("%s json: exit %d stdout %q", name, exitCode(err), stdout)
		}
	}
}

func TestRunOutputSchemaUsageErrors(t *testing.T) {
	dir := t.TempDir()
	notJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(notJSON, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"missing file":          {"run", "--session", "x", "--output-schema", filepath.Join(dir, "none.json"), "hi"},
		"not json":              {"run", "--session", "x", "--output-schema", notJSON, "hi"},
		"not an object":         {"run", "--session", "x", "--output-schema", writeOutputSchema(t, `[]`), "hi"},
		"invalid schema":        {"run", "--session", "x", "--output-schema", writeOutputSchema(t, `{"type":7}`), "hi"},
		"strict without schema": {"run", "--session", "x", "--output-strict=false", "hi"},
	} {
		if _, _, err := runJad(t, "", args...); exitCode(err) != exitUsage {
			t.Errorf("%s: err = %v, want exit %d", name, err, exitUsage)
		}
	}
}
