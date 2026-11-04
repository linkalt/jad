package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	defaultModelFallbackCooldownMS = int64(60_000)
	maxModelFallbackCooldownMS     = int64(86_400_000)
	maxModelFallbackModels         = 8
)

// defaultModelFallbackStatuses are the provider answers that try the next model: rate
// limited, and the server-side failures a provider reports while it is overloaded or down.
var defaultModelFallbackStatuses = []int{429, 500, 502, 503, 504, 529}

var modelFallbackKeys = []string{"models", "on_status", "on_network_error", "cooldown_ms"}

// ModelFallbackSettings is the model_fallback setting: the models a request goes to when
// the session's model fails it after its own retries, before the response sent anything.
type ModelFallbackSettings struct {
	// Models are the fallbacks in the order they are tried.
	Models []ModelFallbackEntry
	// OnStatus are the HTTP statuses that try the next model.
	OnStatus []int
	// OnNetworkError also tries the next model when the request got no answer at all.
	OnNetworkError bool
	// CooldownMS is how long requests skip the session's model after it failed one and go
	// straight to the fallbacks; zero always tries it first.
	CooldownMS int64
}

// ModelFallbackEntry is one fallback model: the model ID as written, and the model file
// and reasoning effort it names once the catalog has resolved it.
type ModelFallbackEntry struct {
	ID              string
	ModelPath       string
	ReasoningEffort string
}

// Falls reports whether an HTTP status tries the next model.
func (s ModelFallbackSettings) Falls(status int) bool {
	for _, candidate := range s.OnStatus {
		if candidate == status {
			return true
		}
	}
	return false
}

// normalizeModelFallback reads model_fallback; absent or null is nil (no fallback).
func normalizeModelFallback(raw json.RawMessage, path string) (*ModelFallbackSettings, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	const section = "model_fallback"
	fields, err := decodeConfigObject(raw, path+": "+section)
	if err != nil {
		return nil, configError("%s: %s must be an object", path, section)
	}
	for key := range fields {
		if !containsString(modelFallbackKeys, key) {
			return nil, unsupportedToolKey(path+": "+section, key, modelFallbackKeys, "")
		}
	}
	settings := &ModelFallbackSettings{OnStatus: append([]int(nil), defaultModelFallbackStatuses...), OnNetworkError: true, CooldownMS: defaultModelFallbackCooldownMS}
	var ids []string
	if err := json.Unmarshal(fields["models"], &ids); err != nil || len(ids) == 0 || len(ids) > maxModelFallbackModels {
		return nil, configError("%s: %s.models must list 1 to %d model IDs", path, section, maxModelFallbackModels)
	}
	seen := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || strings.HasSuffix(id, ".json") {
			return nil, configError("%s: %s.models[%d] must be a model ID (<category>/<name>, optionally @<effort>)", path, section, index)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, configError("%s: %s.models lists %s twice", path, section, id)
		}
		seen[id] = struct{}{}
		settings.Models = append(settings.Models, ModelFallbackEntry{ID: id})
	}
	if rawStatus, exists := fields["on_status"]; exists && strings.TrimSpace(string(rawStatus)) != "null" {
		var statuses []int
		if err := json.Unmarshal(rawStatus, &statuses); err != nil || statuses == nil {
			return nil, configError("%s: %s.on_status must be an array of HTTP statuses", path, section)
		}
		for index, status := range statuses {
			if status < 400 || status > 599 {
				return nil, configError("%s: %s.on_status[%d] must be an HTTP error status (400 to 599)", path, section, index)
			}
		}
		settings.OnStatus = statuses
	}
	if settings.OnNetworkError, err = bashBool(fields, "on_network_error", true, path+": "+section); err != nil {
		return nil, err
	}
	if settings.CooldownMS, err = configuredInteger(fields, "cooldown_ms", defaultModelFallbackCooldownMS, 0, maxModelFallbackCooldownMS, path, section); err != nil {
		return nil, err
	}
	return settings, nil
}
