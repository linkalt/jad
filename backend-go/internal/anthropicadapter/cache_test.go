package anthropicadapter

import (
	"strings"
	"testing"
)

const cacheBody = `{"model":"m","instructions":"rules","input":[
  {"role":"developer","content":"system prompt"},
  {"role":"user","content":"first"},
  {"role":"assistant","content":[{"type":"output_text","text":"answer"}]},
  {"role":"user","content":"second"},
  {"role":"developer","content":"changing state context"}],
 "tools":[{"type":"function","name":"a","parameters":{}},{"type":"function","name":"b","parameters":{}}]}`

func markedPaths(t *testing.T, request anthropicRequest) []string {
	t.Helper()
	var paths []string
	if request.CacheControl != nil {
		paths = append(paths, "top")
	}
	for i, tool := range request.Tools {
		if tool.CacheControl != nil {
			paths = append(paths, "tool"+itoa(i))
		}
	}
	for i, block := range request.System {
		if block.CacheControl != nil {
			paths = append(paths, "system"+itoa(i))
		}
	}
	for m, message := range request.Messages {
		for b, block := range message.Content {
			if block.CacheControl != nil {
				paths = append(paths, "msg"+itoa(m)+"."+itoa(b))
			}
		}
	}
	return paths
}

func TestCacheOffByDefault(t *testing.T) {
	request := translateOK(t, mustConfig(t, `{}`), cacheBody)
	if paths := markedPaths(t, request); len(paths) != 0 {
		t.Fatalf("caching must be off by default, got %v", paths)
	}
	if strings.Contains(asJSON(t, request), "cache_control") {
		t.Fatal("no cache_control may be sent by default")
	}
}

func TestCacheBreakpoints(t *testing.T) {
	cases := map[string]struct {
		config string
		want   string
	}{
		"automatic": {`{"cache":{"automatic":true}}`, "top"},
		"tools":     {`{"cache":{"tools":true}}`, "tool1"},
		"system":    {`{"cache":{"system":true}}`, "system1"},
		// The changing developer text is the last user block, so the history
		// breakpoint must land on "second" (message 2, block 0), not on it.
		"history":  {`{"cache":{"history":true}}`, "msg2.0"},
		"all four": {`{"cache":{"automatic":true,"tools":true,"system":true,"history":true}}`, "top,tool1,system1,msg2.0"},
	}
	for name, tc := range cases {
		request := translateOK(t, mustConfig(t, tc.config), cacheBody)
		if got := strings.Join(markedPaths(t, request), ","); got != tc.want {
			t.Errorf("%s: marked %q, want %q\n%s", name, got, tc.want, asJSON(t, request))
		}
	}
}

func TestCacheMarkerShapeAndTTL(t *testing.T) {
	request := translateOK(t, mustConfig(t, `{"cache":{"automatic":true,"system":true}}`), cacheBody)
	if got := asJSON(t, request.CacheControl); got != `{"type":"ephemeral"}` {
		t.Fatalf("default marker = %s", got)
	}
	request = translateOK(t, mustConfig(t, `{"cache":{"automatic":true,"system":true,"ttl":"1h"}}`), cacheBody)
	if asJSON(t, request.CacheControl) != `{"type":"ephemeral","ttl":"1h"}` ||
		asJSON(t, request.System[len(request.System)-1].CacheControl) != `{"type":"ephemeral","ttl":"1h"}` {
		t.Fatalf("ttl missing from markers: %s", asJSON(t, request))
	}
}

// Anthropic rejects thinking blocks with cache_control and cannot cache empty
// text, and a tool_result is allowed. The history marker must pick a valid block.
func TestHistoryBreakpointSkipsBlocksThatCannotCarryIt(t *testing.T) {
	signed := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"[{\"type\":\"thinking\",\"thinking\":\"t\",\"signature\":\"S\"}]"}`
	body := `{"model":"m","input":[{"role":"user","content":"go"},` + signed + `]}`
	request := translateOK(t, mustConfig(t, `{"cache":{"history":true}}`), body)
	if got := strings.Join(markedPaths(t, request), ","); got != "msg0.0" {
		t.Fatalf("a thinking block must not carry the marker, marked %q: %s", got, asJSON(t, request))
	}
	withResult := `{"model":"m","input":[{"role":"user","content":"go"},
	  {"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
	  {"type":"function_call_output","call_id":"c1","output":"done"}]}`
	request = translateOK(t, mustConfig(t, `{"cache":{"history":true}}`), withResult)
	if got := strings.Join(markedPaths(t, request), ","); got != "msg2.0" {
		t.Fatalf("a tool_result should carry the marker, marked %q: %s", got, asJSON(t, request))
	}
}

func TestHistoryBreakpointWithMidConversationSystem(t *testing.T) {
	config := mustConfig(t, `{"cache":{"history":true},"default_profile":{"mid_conversation_system":true}}`)
	request := translateOK(t, config, cacheBody)
	if got := strings.Join(markedPaths(t, request), ","); got != "msg2.0" {
		t.Fatalf("a role=system message must be skipped, marked %q: %s", got, asJSON(t, request))
	}
}

func TestHistoryBreakpointNeverSplitsAnEmptyConversationOfOnlyContext(t *testing.T) {
	// Nothing but folded text after a first user turn that is itself empty of
	// cacheable content: no marker is placed rather than an invalid one.
	if got := lastHistoryBlock([]anthropicMessage{{Role: "user", Content: []anthropicBlock{{Type: "text", Text: "ctx", folded: true}}}}); got != nil {
		t.Fatalf("a folded-only message must not be marked: %v", got)
	}
}

func TestCacheConfigValidation(t *testing.T) {
	if _, err := LoadConfig(writeConfig(t, `{"cache":{"ttl":"2h"}}`)); err == nil || !strings.Contains(err.Error(), "cache.ttl") {
		t.Fatalf("bad ttl: %v", err)
	}
	if _, err := LoadConfig(writeConfig(t, `{"cache":{"ttl":"1h","system":true}}`)); err != nil {
		t.Fatalf("1h ttl rejected: %v", err)
	}
	if _, err := LoadConfig(writeConfig(t, `{"cache":{"bogus":true}}`)); err == nil {
		t.Fatal("an unknown cache key must be rejected")
	}
}

// The translation never produces empty text or an unflagged role=system block,
// so these two rules are checked on the function directly.
func TestLastHistoryBlockRules(t *testing.T) {
	messages := []anthropicMessage{
		{Role: "user", Content: []anthropicBlock{{Type: "text", Text: "real"}}},
		{Role: "assistant", Content: []anthropicBlock{{Type: "text", Text: ""}}},
		{Role: "system", Content: []anthropicBlock{{Type: "text", Text: "note"}}},
	}
	got := lastHistoryBlock(messages)
	if got == nil || got.Text != "real" {
		t.Fatalf("empty text and role=system blocks must be skipped, got %+v", got)
	}
}
