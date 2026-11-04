package sessionfs

import (
	"bytes"
	"encoding/json"

	"jad/backend-go/internal/anthropicadapter"
	"jad/backend-go/internal/chatadapter"
)

// modelAPIFields reads api and its settings object. A model in api anthropic_messages or
// chat_completions is served by a translator jad runs in process; its object (anthropic
// or chat_completions) is checked here, so a mistake fails the load and jad check, not
// the first run.
func modelAPIFields(fields map[string]json.RawMessage, path, baseURL, authMode string) (string, json.RawMessage, json.RawMessage, error) {
	api, err := stringField(fields, "api", APIResponses, path)
	if err != nil {
		return "", nil, nil, err
	}
	if api != APIResponses && api != APIAnthropicMessages && api != APIChatCompletions {
		return "", nil, nil, configError("%s: api must be %s, %s or %s", path, APIResponses, APIAnthropicMessages, APIChatCompletions)
	}
	if api != APIResponses && authMode != "api_key" {
		return "", nil, nil, configError("%s: auth_mode %s needs api responses", path, authMode)
	}
	object := func(key, owner string) (json.RawMessage, error) {
		raw, ok := fields[key]
		if !ok || string(bytes.TrimSpace(raw)) == "null" {
			return nil, nil
		}
		if api != owner {
			return nil, configError("%s: %s needs api %s", path, key, owner)
		}
		return bytes.TrimSpace(raw), nil
	}
	anthropic, err := object("anthropic", APIAnthropicMessages)
	if err != nil {
		return "", nil, nil, err
	}
	chat, err := object("chat_completions", APIChatCompletions)
	if err != nil {
		return "", nil, nil, err
	}
	switch api {
	case APIAnthropicMessages:
		if _, err := anthropicadapter.ParseEmbeddedConfig(anthropic, baseURL); err != nil {
			return "", nil, nil, configError("%s: anthropic: %s", path, err.Error())
		}
	case APIChatCompletions:
		if _, err := chatadapter.ParseConfig(chat, baseURL); err != nil {
			return "", nil, nil, configError("%s: chat_completions: %s", path, err.Error())
		}
	}
	return api, anthropic, chat, nil
}
