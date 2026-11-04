package session

import (
	"encoding/json"
	"testing"
)

func TestBuildRequestSendsThePromptCacheKeyOnlyWhenSet(t *testing.T) {
	history := [][]byte{[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`)}
	for _, test := range []struct {
		key  string
		want string
	}{{"", ""}, {"jad-0123", "jad-0123"}} {
		request, err := BuildRequest(ModelSettings{Model: "gpt-test", Request: json.RawMessage(`{"store":false}`), PromptCacheKey: test.key}, history)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(request, &fields); err != nil {
			t.Fatal(err)
		}
		raw, exists := fields["prompt_cache_key"]
		if test.want == "" {
			if exists {
				t.Fatalf("an empty key was sent: %s", raw)
			}
			continue
		}
		var got string
		if err := json.Unmarshal(raw, &got); err != nil || got != test.want || string(fields["store"]) != "false" {
			t.Fatalf("request = %s", request)
		}
	}
}
