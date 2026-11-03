package app

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// TestARunAfterTheStoreWasDroppedContinuesTheTranscript runs a session, lets its idle
// store leave the cache, and runs it again: the reopened store must append the second
// turn after the first, with the turn counter carried over from disk.
func TestARunAfterTheStoreWasDroppedContinuesTheTranscript(t *testing.T) {
	responses := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses++
		w.Header().Set("Content-Type", "text/event-stream")
		item := fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}}`, responses)
		completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9},"output":[]}}`, responses)
		for _, payload := range []string{item, completed, "[DONE]"} {
			fmt.Fprintf(w, "data: %s\n\n", payload)
		}
	}))
	defer provider.Close()
	config, fixture := layoutConfig(t, layouttest.Spec{
		Server:   map[string]any{"session_store_idle_ms": 20},
		Settings: map[string]any{"persistence": map[string]any{"durability": "process", "max_item_bytes": 1048576}},
		Models:   map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "k"})
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() { service.Close(); api.Close() }()
	location, err := sessionfs.Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		response := postRun(t, api.URL+"/api/sessions/"+fixture.SessionID+"/run", integrationAPIToken)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "event: completed") {
			t.Fatalf("run: status %d, body %s", response.StatusCode, body)
		}
	}
	run()
	// The run registry's own retention lets the run end first; then the store's last
	// use is over and the idle drop follows.
	waitForDrop(t, service.resolver, session)
	run()
	file, err := os.Open(filepath.Clean(session.MessagesPath))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) != 4 {
		t.Fatalf("transcript has %d lines, want input+response twice:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for index, want := range []string{`"text":"hello"`, `"session_turn":1`, `"text":"hello"`, `"session_turn":2`} {
		if !strings.Contains(lines[index], want) {
			t.Fatalf("line %d = %s, want it to contain %s", index+1, lines[index], want)
		}
	}
	if !strings.Contains(lines[3], `"answer 2"`) {
		t.Fatalf("the second response is not the second provider answer: %s", lines[3])
	}
}
