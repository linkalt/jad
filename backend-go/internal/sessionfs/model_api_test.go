package sessionfs

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadModelAPIDefaultsAndValues(t *testing.T) {
	model, err := hostedToolsModel(t, ``)
	if err != nil || model.API != APIResponses || model.Anthropic != nil || model.ChatCompletions != nil {
		t.Fatalf("default: %+v %v", model, err)
	}
	model, err = hostedToolsModel(t, `,"api":"anthropic_messages","anthropic":{"default_max_tokens":4096}`)
	if err != nil || model.API != APIAnthropicMessages || string(model.Anthropic) != `{"default_max_tokens":4096}` || model.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("anthropic: %+v %v", model, err)
	}
	model, err = hostedToolsModel(t, `,"api":"chat_completions","base_url":"http://localhost:11434/v1","chat_completions":{"max_tokens_field":"max_tokens"}`)
	if err != nil || model.API != APIChatCompletions || string(model.ChatCompletions) != `{"max_tokens_field":"max_tokens"}` || model.APIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("chat: %+v %v", model, err)
	}
	// Without its object a translated model uses the adapter's defaults.
	if _, err := hostedToolsModel(t, `,"api":"anthropic_messages"`); err != nil {
		t.Fatalf("anthropic defaults: %v", err)
	}
}

func TestLoadModelAPIRefusals(t *testing.T) {
	for name, test := range map[string]struct{ extra, want string }{
		"unknown api":         {`,"api":"messages"`, "api must be"},
		"api not a string":    {`,"api":1`, "api"},
		"object of other api": {`,"api":"chat_completions","anthropic":{}`, "anthropic needs api anthropic_messages"},
		"object without api":  {`,"chat_completions":{}`, "chat_completions needs api chat_completions"},
		"oauth":               {`,"api":"chat_completions","auth_mode":"openai_oauth"`, "needs api responses"},
		"hosted tools":        {`,"api":"anthropic_messages","hosted_tools":[{"type":"web_search"}]`, "hosted_tools needs api responses"},
		"bad adapter key":     {`,"api":"anthropic_messages","anthropic":{"listen":"127.0.0.1:1"}`, "anthropic: listen is not used"},
		"bad adapter value":   {`,"api":"anthropic_messages","anthropic":{"cache":{"ttl":"2h"}}`, "anthropic: config: cache.ttl"},
		"bad chat key":        {`,"api":"chat_completions","chat_completions":{"usage":true}`, "chat_completions: parse"},
		"bad chat value":      {`,"api":"chat_completions","chat_completions":{"system_role":"user"}`, "chat_completions: system_role"},
	} {
		_, err := hostedToolsModel(t, test.extra)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

// The sample model files load: the Claude model of sample/jad and the Chat Completions example.
func TestSampleTranslatedModelsLoad(t *testing.T) {
	for path, api := range map[string]string{
		filepath.Join("..", "..", "..", "sample", "jad", "models", "cheap", "haiku.json"): APIAnthropicMessages,
		filepath.Join("..", "..", "..", "sample", "model-chat-completions.json"):          APIChatCompletions,
	} {
		model, err := loadModel(path, "", "")
		if err != nil || model.API != api {
			t.Errorf("%s: api %q, err %v", path, model.API, err)
		}
	}
}
