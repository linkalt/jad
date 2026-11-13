package app

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/remoteapi"
)

// remoteLoop makes a workspace-layout server call itself. Four profiles, p1 to p4, each in
// both workspaces, have the profile_message tool and two bindings, self-a and self-b, that point
// at the server's own address, one for each workspace; each binding has a key and the server has
// a token row for it. The fake provider tells which profile is asking by its model and has
// p1 message p2 of b:s1, p2 message p3 of a:s1, p3 message p4 of b:s1 and p4 message p1 of a:s1
// again: a cycle through the remote API whose last step comes back to the profile that started it.
type remoteLoop struct {
	mu     sync.Mutex
	models []string
	// leaked is set when a request to the provider holds one of the loop's secrets; first is the
	// first request body, which holds the tool descriptions the model sees.
	leaked bool
	first  string
	// noKeys leaves remote_keys out of workspace.json, so the bindings are listed but unkeyed.
	noKeys bool
	// workspaceB is the profile_message block (JSON object text) of workspace b's workspace.json.
	workspaceB string
	// global is extra text for global.json (a leading comma included).
	global string
}

const (
	loopTokenA = "JADREMOTE_loop_a_0123456789abcdef0123456789ab"
	loopTokenB = "JADREMOTE_loop_b_0123456789abcdef0123456789ab"
)

// loopRows are the token rows of the loop: one for each workspace, and peer to start it in a.
func loopRows() string {
	return tokenTable(workspaceRow("peer", remoteTestSecret, "a", fullGrant), workspaceRow("self-a", loopTokenA, "a", fullGrant), workspaceRow("self-b", loopTokenB, "b", fullGrant))
}

// loopNext is where the profile of each model sends its message.
var loopNext = map[string]string{
	"model-p1": "remote:self-b/s1/dev/p2",
	"model-p2": "remote:self-a/s1/dev/p3",
	"model-p3": "remote:self-b/s1/dev/p4",
	"model-p4": "remote:self-a/s1/dev/p1",
}

// answer is the output item for a request: the profile_message call on the first request of a
// run, the plain item once the call has its result.
func (l *remoteLoop) answer(raw []byte, plain string) string {
	var request struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &request)
	l.mu.Lock()
	l.models = append(l.models, request.Model)
	if l.first == "" {
		l.first = string(raw)
	}
	for _, secret := range []string{loopTokenA, loopTokenB} {
		l.leaked = l.leaked || strings.Contains(string(raw), secret)
	}
	l.mu.Unlock()
	target, ok := loopNext[request.Model]
	if !ok || strings.Contains(string(raw), "function_call_output") {
		return plain
	}
	arguments, _ := json.Marshal(map[string]any{"profile": target, "message": "round and round", "block_ms": 20000})
	item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call-1", "name": "profile_message", "arguments": string(arguments)})
	return string(item)
}

func (l *remoteLoop) requests(model string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.models {
		if m == model {
			n++
		}
	}
	return n
}

// prepare writes what the loop needs on top of the two workspaces: the tool definitions, one
// toolset, the four models and profiles and the remote entry whose address is the one the
// server will listen on. It returns that listener.
func (l *remoteLoop) prepare(t *testing.T, put func(path, text string), global, providerURL, a, b string) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	definitions := make([]string, 0, 2)
	for _, name := range []string{"profile_message", "async"} {
		definition, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, definition)
	}
	libraries, _ := json.Marshal(definitions)
	put(filepath.Join(global, "global.json"), `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}],"libraries":{"tool_definition_paths":`+string(libraries)+`}`+l.global+`}`)
	put(filepath.Join(global, "tools", "coding", "loop.json"), `{"native":{"profile_message":{"remotes":["self-a","self-b"]}},"mcp":{}}`)
	for _, binding := range []string{"a", "b"} {
		put(filepath.Join(global, "remotes", "self-"+binding+".json"), `{"kind":"remote","remote":{"url":"http://`+listener.Addr().String()+`","workspace":"`+binding+`","timeout_ms":5000,"poll_ms":100,"lost_after_ms":3000,"retries":1}}`)
	}
	// Key a holds its token; key b reads it from the environment (JAD_REMOTE_E2E).
	put(filepath.Join(global, "auth", "keys", "key-a.json"), `{"kind":"remote_key","binding":"self-a","token":"`+loopTokenA+`"}`)
	put(filepath.Join(global, "auth", "keys", "key-b.json"), `{"kind":"remote_key","binding":"self-b","token_env":"JAD_REMOTE_LOOP_B"}`)
	if !l.noKeys {
		put(filepath.Join(a, ".jad", "workspace.json"), `{"remote_keys":["key-a","key-b"]}`)
		put(filepath.Join(b, ".jad", "workspace.json"), `{"remote_keys":["key-a","key-b"]`+l.workspaceB+`}`)
	}
	for _, name := range []string{"p1", "p2", "p3", "p4"} {
		put(filepath.Join(global, "models", "openai", name+".json"), `{"provider":"openai","model":"model-`+name+`","api_key_env":"FIXTURE_KEY","base_url":"`+providerURL+`/v1"}`)
		for _, dir := range []string{a, b} {
			put(filepath.Join(dir, ".jad", "profiles", "dev", name+".json"), `{"models":["openai/`+name+`"],"model_default":"openai/`+name+`","toolset":"coding/loop"}`)
		}
	}
	return listener
}

// A message that goes round through the server's own bindings, each with its key, p1 of a:s1 to p2 of b:s1 to
// p3 of a:s1 to p4 of b:s1 and back to p1 of a:s1, stops at the hop limit: the last call is
// refused before it reaches the busy p1, every run ends, and no profile gets a second run.
func TestRemoteLoopThroughTheServersOwnEntryStopsAtTheHopLimit(t *testing.T) {
	loop := &remoteLoop{}
	w := startRemoteWorkspacesWith(t, loopRows(), loop)
	status, body := w.call(t, "POST", remoteapi.Prefix+"/messages", remoteTestSecret, map[string]any{
		"session": "a:s1", "profile": "dev/p1", "message": "start the loop", "request_id": "req-loop-00001"})
	if status != http.StatusAccepted {
		t.Fatalf("start: %d %v", status, body)
	}
	if done := w.waitDone(t, body["id"].(string)); done["state"] != "done" {
		t.Fatalf("the first run of the loop did not end well: %v", done)
	}
	for _, model := range []string{"model-p1", "model-p2", "model-p3", "model-p4"} {
		// One request makes the call and one reads its result, so a second run would show twice more.
		if n := loop.requests(model); n != 2 {
			t.Errorf("%s was asked %d times, want 2 (one run, one call)", model, n)
		}
	}
	last, _ := os.ReadFile(filepath.Join(w.b, ".jad", "state", "s1", "dev", "p4", "messages.jsonl"))
	if !strings.Contains(string(last), "remote_hop_limit") {
		t.Fatalf("the call that would close the loop was not refused at the hop limit:\n%s", last)
	}
	first, _ := os.ReadFile(filepath.Join(w.a, ".jad", "state", "s1", "dev", "p1", "messages.jsonl"))
	if strings.Count(string(first), `"role":"user"`) != 1 {
		t.Fatalf("p1 got a second message from the loop:\n%s", first)
	}
	// Neither key's token is in any request to the provider, nor in any file the runs wrote.
	loop.mu.Lock()
	leaked := loop.leaked
	loop.mu.Unlock()
	if leaked {
		t.Fatal("a token reached the provider")
	}
	assertNoSecretUnder(t, []string{loopTokenA, loopTokenB}, w.a, w.b)
}

// assertNoSecretUnder fails when any file under the folders holds one of the secrets.
func assertNoSecretUnder(t *testing.T, secrets []string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			data, _ := os.ReadFile(path)
			for _, secret := range secrets {
				if strings.Contains(string(data), secret) {
					t.Errorf("%s holds a secret", path)
				}
			}
			return nil
		})
	}
}
