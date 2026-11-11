package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"jad/backend-go/internal/openaiapi"
	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// modelShape is what of a model the request body carries: its name, the request defaults
// its file adds and the hosted tools that follow the function tools.
type modelShape struct {
	name        string
	request     json.RawMessage
	hostedTools []json.RawMessage
}

func shapeOf(model sessionfs.Model) modelShape {
	return modelShape{name: model.Name, request: model.Request, hostedTools: model.HostedTools}
}

// fallbackTarget is one fallback model with its own provider client.
type fallbackTarget struct {
	id       string
	shape    modelShape
	streamer responses.Streamer
}

// fallbackStreamer sends a request to the session's model and, when that fails in a way
// model_fallback names before any event of the response arrived, to each fallback in
// turn. The provider client's own retries run first, inside each Stream. Once the
// response has sent an event nothing is retried elsewhere: the run already holds part of
// that response. After the session's model failed, requests skip it for cooldown_ms.
type fallbackStreamer struct {
	primary  responses.Streamer
	shape    modelShape
	targets  []fallbackTarget
	settings sessionfs.ModelFallbackSettings
	logger   *slog.Logger
	now      func() time.Time

	mu        sync.Mutex
	downUntil time.Time
}

func (f *fallbackStreamer) Stream(ctx context.Context, body json.RawMessage, handle responses.EventHandler) error {
	var last error
	skipped := !f.primaryUp()
	if !skipped {
		delivered, err := f.try(ctx, f.primary, body, handle)
		if !f.fallsBack(ctx, err, delivered) {
			return err
		}
		f.markDown()
		f.logger.Warn("model request failed; trying the fallback models", "model", f.shape.name, "status", providerStatus(err), "fallbacks", len(f.targets))
		last = err
	}
	for _, target := range f.targets {
		rewritten, err := rewriteForModel(body, f.shape, target.shape)
		if err != nil {
			return fmt.Errorf("model_fallback: build the request for %s: %w", target.id, err)
		}
		delivered, err := f.try(ctx, target.streamer, rewritten, handle)
		if err == nil {
			f.logger.Info("model request answered by a fallback model", "model", f.shape.name, "fallback", target.id)
		}
		if !f.fallsBack(ctx, err, delivered) {
			return err
		}
		f.logger.Warn("fallback model failed", "fallback", target.id, "status", providerStatus(err))
		last = err
	}
	if skipped {
		// Every fallback failed while the session's model was cooling down: it is tried
		// after all, so the request is not refused without asking it.
		_, err := f.try(ctx, f.primary, body, handle)
		return err
	}
	return last
}

// try streams through one client and reports whether any event reached the handler.
func (f *fallbackStreamer) try(ctx context.Context, streamer responses.Streamer, body json.RawMessage, handle responses.EventHandler) (bool, error) {
	delivered := false
	err := streamer.Stream(ctx, body, func(event responses.Event) error {
		delivered = true
		return handle(event)
	})
	return delivered, err
}

// fallsBack reports whether a failure tries the next model: the response sent nothing,
// the run was not stopped, and the failure is a status on_status lists or, with
// on_network_error, a request that got no HTTP answer.
func (f *fallbackStreamer) fallsBack(ctx context.Context, err error, delivered bool) bool {
	if err == nil || delivered || ctx.Err() != nil {
		return false
	}
	if status := providerStatus(err); status != 0 {
		return f.settings.Falls(status)
	}
	return f.settings.OnNetworkError && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (f *fallbackStreamer) primaryUp() bool {
	if f.settings.CooldownMS <= 0 {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.now().Before(f.downUntil)
}

func (f *fallbackStreamer) markDown() {
	if f.settings.CooldownMS <= 0 {
		return
	}
	f.mu.Lock()
	f.downUntil = f.now().Add(time.Duration(f.settings.CooldownMS) * time.Millisecond)
	f.mu.Unlock()
}

// providerStatus is the HTTP status of a provider failure, or 0 for one without.
func providerStatus(err error) int {
	var status *openaiapi.StatusError
	if errors.As(err, &status) {
		return status.Status
	}
	return 0
}

// requestOwnedKeys are the body fields the runner writes itself; a model's request
// defaults never set them.
var requestOwnedKeys = map[string]bool{"model": true, "input": true, "stream": true, "tools": true}

// rewriteForModel turns a request built for one model into the same request for another:
// the model name, the request defaults of its file in place of the first model's, and its
// hosted tools in place of the first model's at the end of tools. The input, and with it
// the session's prompt, is unchanged.
func rewriteForModel(body json.RawMessage, from, to modelShape) (json.RawMessage, error) {
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("the request is not a JSON object")
	}
	for key := range decodeObject(from.request) {
		if !requestOwnedKeys[key] {
			delete(fields, key)
		}
	}
	for key, value := range decodeObject(to.request) {
		if !requestOwnedKeys[key] {
			fields[key] = value
		}
	}
	name, err := json.Marshal(to.name)
	if err != nil {
		return nil, err
	}
	fields["model"] = name
	var tools []json.RawMessage
	if raw, ok := fields["tools"]; ok {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, errors.New("the request's tools are not an array")
		}
	}
	tools = withoutHostedTools(tools, from.hostedTools)
	tools = append(tools, to.hostedTools...)
	if len(tools) == 0 {
		delete(fields, "tools")
	} else if fields["tools"], err = json.Marshal(tools); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func decodeObject(raw json.RawMessage) map[string]json.RawMessage {
	fields := make(map[string]json.RawMessage)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &fields)
	}
	return fields
}

// withoutHostedTools drops the hosted tools the runner appended after the function tools.
func withoutHostedTools(tools, hosted []json.RawMessage) []json.RawMessage {
	if len(hosted) == 0 || len(hosted) > len(tools) {
		return tools
	}
	tail := tools[len(tools)-len(hosted):]
	for index := range hosted {
		if !sameJSON(tail[index], hosted[index]) {
			return tools
		}
	}
	return tools[:len(tools)-len(hosted)]
}

func sameJSON(a, b json.RawMessage) bool {
	var left, right bytes.Buffer
	if json.Compact(&left, a) != nil || json.Compact(&right, b) != nil {
		return false
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}

// runStreamer is the provider client of a run: the model's own, wrapped with the session's
// model_fallback when it has one. A fallback whose model file or credential cannot be
// used is left out with a warning, so a missing spare key never stops the session's own
// model; a fallback that names the session's model is left out too. The string is the
// fallback part of the runner's identity (planRunStreamer).
func (r *Resolver) runStreamer(config sessionfs.SessionConfig, apiKey string) (responses.Streamer, string) {
	plan := r.planRunStreamer(config, apiKey)
	return r.buildRunStreamer(plan), plan.identity
}

// streamerPlan is a run's provider client before it is built: the fallback model files
// and keys are loaded, so the runner's identity is known, but no client exists and nothing
// is logged yet. prepareRun plans on every run and builds only when it creates a runner,
// so a cached runner costs no client and a fallback that cannot be used is reported once
// per runner, not on every run.
type streamerPlan struct {
	config   sessionfs.SessionConfig
	apiKey   string
	targets  []plannedFallback
	skipped  []skippedFallback
	identity string
}

// plannedFallback is a fallback model whose file and credential loaded.
type plannedFallback struct {
	id     string
	config sessionfs.SessionConfig
	apiKey string
}

// skippedFallback is a fallback left out, with the warning buildRunStreamer logs for it.
type skippedFallback struct {
	message string
	args    []any
}

// runModelKeys are the API key variables (api_key_env) of the models that may answer a run of
// config: its own model's and each model_fallback model's that loads, which planRunStreamer may
// send a request to (T-009). A child process of the run's calls never gets them
// (tools.ExecutionContext.ModelKeyEnvs). A fallback that does not load is no target, so its key
// is not listed.
func runModelKeys(config sessionfs.SessionConfig) []string {
	keys := tools.JoinDeny(nil, []string{config.Model.APIKeyEnv})
	if config.ModelFallback != nil {
		for _, entry := range config.ModelFallback.Models {
			fallback := config
			fallback.ModelPath, fallback.ReasoningEffort = entry.ModelPath, entry.ReasoningEffort
			if loaded, err := fallback.WithModel(); err == nil {
				keys = tools.JoinDeny(keys, []string{loaded.Model.APIKeyEnv})
			}
		}
	}
	return slices.DeleteFunc(keys, func(key string) bool { return key == "" })
}

// planRunStreamer loads the session's model_fallback entries. The identity is empty when
// no fallback can be used (the run streams from its own model alone), and otherwise the
// model_fallback settings followed by each usable fallback's runner fingerprint.
func (r *Resolver) planRunStreamer(config sessionfs.SessionConfig, apiKey string) streamerPlan {
	plan := streamerPlan{config: config, apiKey: apiKey}
	if config.ModelFallback == nil {
		return plan
	}
	identity := ""
	for _, entry := range config.ModelFallback.Models {
		if entry.ModelPath == config.ModelPath && entry.ReasoningEffort == config.ReasoningEffort {
			continue
		}
		fallback := config
		fallback.ModelPath, fallback.ReasoningEffort = entry.ModelPath, entry.ReasoningEffort
		loaded, err := fallback.WithModel()
		if err != nil {
			plan.skipped = append(plan.skipped, skippedFallback{message: "model_fallback: model left out", args: []any{"model", entry.ID, "error", err}})
			continue
		}
		key, err := r.providerAPIKey(loaded)
		if err != nil {
			plan.skipped = append(plan.skipped, skippedFallback{message: "model_fallback: model left out", args: []any{"model", entry.ID, "error", err}})
			continue
		}
		if config.MaxRunCostUSD > 0 && loaded.Model.Pricing == nil {
			// Its responses could not be counted against the cap.
			plan.skipped = append(plan.skipped, skippedFallback{message: "model_fallback: model left out: max_run_cost_usd is set and the model file has no pricing", args: []any{"model", entry.ID}})
			continue
		}
		plan.targets = append(plan.targets, plannedFallback{id: entry.ID, config: loaded, apiKey: key})
		identity += "|" + fingerprintRunner(loaded, key)
	}
	if len(plan.targets) > 0 {
		settings, _ := json.Marshal(config.ModelFallback)
		plan.identity = string(settings) + identity
	}
	return plan
}

// buildRunStreamer builds the provider clients of a plan and logs the fallbacks it left out.
func (r *Resolver) buildRunStreamer(plan streamerPlan) responses.Streamer {
	for _, skipped := range plan.skipped {
		r.logger.Warn(skipped.message, skipped.args...)
	}
	primary := withPricing(r.newProviderStreamer(plan.config, plan.apiKey), plan.config.Model)
	if len(plan.targets) == 0 {
		return primary
	}
	streamer := &fallbackStreamer{primary: primary, shape: shapeOf(plan.config.Model), settings: *plan.config.ModelFallback, logger: r.logger, now: time.Now}
	for _, target := range plan.targets {
		streamer.targets = append(streamer.targets, fallbackTarget{id: target.id, shape: shapeOf(target.config.Model), streamer: withPricing(r.newProviderStreamer(target.config, target.apiKey), target.config.Model)})
	}
	return streamer
}
