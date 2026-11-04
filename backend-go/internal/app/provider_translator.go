package app

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"jad/backend-go/internal/anthropicadapter"
	"jad/backend-go/internal/chatadapter"
	"jad/backend-go/internal/sessionfs"
)

// inProcessBaseURL is the base URL the Responses client is given when an in-process
// translator serves the model. The host never resolves: the in-process transport
// answers every request itself.
const inProcessBaseURL = "http://in-process.invalid/v1"

// credentialFilePlaceholderKey is handed to the Responses client of an Anthropic model
// whose adapter reads its token from upstream.credential_file: the adapter ignores it.
const credentialFilePlaceholderKey = "anthropic-credential-file"

// providerTranslator returns the in-process handler that serves the Responses API for a
// model whose provider speaks another protocol, or nil for api responses. upstream is
// the client the translator calls the provider with; nil means a plain client. The model
// file was validated at load time, so a parse error here means it changed in between,
// and the run fails on the translator's first answer rather than here.
func providerTranslator(config sessionfs.SessionConfig, upstream *http.Client, logger *slog.Logger) http.Handler {
	if upstream == nil {
		upstream = &http.Client{}
	}
	switch config.Model.API {
	case sessionfs.APIAnthropicMessages:
		adapterConfig, err := anthropicadapter.ParseEmbeddedConfig(config.Model.Anthropic, config.Model.BaseURL)
		if err != nil {
			return refusedTranslator("anthropic: " + err.Error())
		}
		handler := anthropicadapter.NewHandler(adapterConfig, upstream)
		handler.SetLogger(logger)
		return handler
	case sessionfs.APIChatCompletions:
		chatConfig, err := chatadapter.ParseConfig(config.Model.ChatCompletions, config.Model.BaseURL)
		if err != nil {
			return refusedTranslator("chat_completions: " + err.Error())
		}
		return chatadapter.NewHandler(chatConfig, upstream, logger)
	}
	return nil
}

// anthropicHoldsCredential reports whether the model's adapter settings read the token
// from upstream.credential_file.
func anthropicHoldsCredential(config sessionfs.SessionConfig) bool {
	adapterConfig, err := anthropicadapter.ParseEmbeddedConfig(config.Model.Anthropic, config.Model.BaseURL)
	return err == nil && adapterConfig.HoldsCredential()
}

// refusedTranslator answers 422 with the settings error, which the run reports.
func refusedTranslator(message string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "invalid_model_settings", "message": message}})
	})
}
