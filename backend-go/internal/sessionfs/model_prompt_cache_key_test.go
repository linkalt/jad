package sessionfs

import (
	"strings"
	"testing"
)

func TestModelPromptCacheKeyDefaultsOffAndAcceptsSession(t *testing.T) {
	model, err := hostedToolsModel(t, "")
	if err != nil || model.PromptCacheKey != PromptCacheKeyOff {
		t.Fatalf("default = %q, %v; want off", model.PromptCacheKey, err)
	}
	for _, value := range []string{"off", "session"} {
		model, err := hostedToolsModel(t, `,"prompt_cache_key":"`+value+`"`)
		if err != nil || model.PromptCacheKey != value {
			t.Fatalf("%s: got %q, %v", value, model.PromptCacheKey, err)
		}
	}
	// off needs no particular api, and a fixed request key stays allowed with it.
	if _, err := hostedToolsModel(t, `,"api":"chat_completions","prompt_cache_key":"off","request":{"prompt_cache_key":"fixed"}`); err != nil {
		t.Fatal(err)
	}
}

func TestModelPromptCacheKeyRejectsBadValues(t *testing.T) {
	cases := map[string]struct{ extra, want string }{
		"unknown value":  {`,"prompt_cache_key":"always"`, `prompt_cache_key must be "off" or "session"`},
		"not a string":   {`,"prompt_cache_key":true`, "prompt_cache_key"},
		"anthropic api":  {`,"api":"anthropic_messages","prompt_cache_key":"session"`, `prompt_cache_key "session" needs api responses`},
		"chat api":       {`,"api":"chat_completions","prompt_cache_key":"session"`, `prompt_cache_key "session" needs api responses`},
		"also a request": {`,"prompt_cache_key":"session","request":{"prompt_cache_key":"fixed"}`, "request.prompt_cache_key both set the key"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := hostedToolsModel(t, test.extra)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
