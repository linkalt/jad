package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
)

// pinWorkspace is a workspace whose profile dev/coder runs its own model openai/fast with its own
// openai/spare as model_fallback, both based on the global openai/default (key FIXTURE_KEY);
// openai/other is a second global model with the key OTHER_KEY. Both answer from provider.
// sessionSettings are the session file's settings.
func pinWorkspace(t *testing.T, provider *recordingProvider, sessionSettings string) (*Service, string) {
	t.Helper()
	return pinWorkspaceIn(t, t.TempDir(), provider, sessionSettings)
}

// pinWorkspaceIn is pinWorkspace with the home and the workspace (base/w) in base.
func pinWorkspaceIn(t *testing.T, base string, provider *recordingProvider, sessionSettings string) (*Service, string) {
	t.Helper()
	server := httptest.NewServer(provider.handler())
	t.Cleanup(server.Close)
	home := filepath.Join(base, "home")
	w := filepath.Join(base, "w")
	put := func(path, text string) { writeWorkspaceTestFile(t, path, text) }
	put(filepath.Join(home, ".jad", "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY","base_url":"`+server.URL+`/v1"}`)
	put(filepath.Join(home, ".jad", "models", "openai", "other.json"), `{"provider":"openai","model":"gpt-other","api_key_env":"OTHER_KEY","base_url":"`+server.URL+`/v1"}`)
	put(filepath.Join(w, ".jad", "workspace.json"), `{}`)
	put(filepath.Join(w, ".jad", "sessions", "s1.json"), `{"schema_version":1,"settings":`+sessionSettings+`}`)
	put(filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default","profile:openai/fast","profile:openai/spare"],"model_default":"profile:openai/fast","settings":{"model_fallback":{"models":["profile:openai/spare"]}}}`)
	put(pinOwnModel(w, "fast"), pinOwnModelFile("global:openai/default", "0.2"))
	put(pinOwnModel(w, "spare"), pinOwnModelFile("global:openai/default", "0.2"))
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: w, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken, "HOME": home, "FIXTURE_KEY": "fixture-key", "OTHER_KEY": "other-key"}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service, w
}

func pinOwnModel(w, name string) string {
	return filepath.Join(w, ".jad", "profile-own", "dev", "coder", "models", "openai", name+".json")
}

func pinOwnModelFile(basedOn, temperature string) string {
	return `{"based_on":"` + basedOn + `","set":{"request":{"temperature":` + temperature + `}}}`
}

func requestTemperature(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var request struct {
		Temperature *json.Number `json:"temperature"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Temperature == nil {
		t.Fatalf("no temperature in %s", raw)
	}
	return request.Temperature.String()
}

// A prompt_submit command that rewrites the run's own model (its set, then its based_on) runs
// after the run started and before it loads its model: the run still asks the provider with the
// model it read when it started, and does not fail with "load the session again". The next run
// uses the edit (decider 3c.2b).
func TestARunUsesTheOwnModelItReadWhenItStarted(t *testing.T) {
	for _, edit := range []struct{ name, file, want string }{
		{"set", pinOwnModelFile("global:openai/default", "0.9"), "0.9"},
		{"based_on", pinOwnModelFile("global:openai/other", "0.7"), "0.7"},
	} {
		t.Run(edit.name, func(t *testing.T) {
			provider := &recordingProvider{}
			w := t.TempDir()
			rewrite := filepath.Join(w, "rewrite.json")
			writeWorkspaceTestFile(t, rewrite, edit.file)
			// The workspace is made inside base, so the command can name the own file before it exists.
			base := t.TempDir()
			hook := runHookEntry("edit", "cp '"+rewrite+"' '"+pinOwnModel(filepath.Join(base, "w"), "fast")+"'")
			settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"enabled": true, "prompt_submit": []any{hook}}})
			service, _ := pinWorkspaceIn(t, base, provider, string(settings))
			run := func() string {
				t.Helper()
				resolved, err := service.resolver.Resolve(context.Background(), "s1", "dev/coder", "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := resolved.Runner.Run(context.Background(), json.RawMessage(`"hello"`), session.RunOptions{}); err != nil {
					t.Fatal(err)
				}
				requests := provider.requests()
				return string(requests[len(requests)-1]["temperature"])
			}
			if got := run(); got != "0.2" {
				t.Fatalf("the run asked with temperature %s, not the 0.2 it started with", got)
			}
			// The next run starts with the edit (the hook writes the same file again).
			if got := run(); got != edit.want {
				t.Fatalf("the next run asked with temperature %s, want %s", got, edit.want)
			}
		})
	}
}

// The fallback plan, the key variables the run's tools and hooks hide, the hooks' own key and a
// sub-agent that keeps the run's model all come from the copy the run took when it started; an
// edit of the own models in between reaches none of them.
func TestTheFallbackHooksAndSubAgentsUseTheRunsCopy(t *testing.T) {
	service, w := pinWorkspace(t, &recordingProvider{}, `{}`)
	resolver := service.resolver
	resolved, err := resolver.Resolve(context.Background(), "s1", "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	config := resolved.Runner.(boundRunner).config
	pinned := config.PinOwnModels()
	put := func(name, text string) { writeWorkspaceTestFile(t, pinOwnModel(w, name), text) }
	put("fast", pinOwnModelFile("global:openai/other", "0.9"))
	put("spare", pinOwnModelFile("global:openai/other", "0.9"))

	loaded, err := pinned.WithModel()
	if err != nil {
		t.Fatal(err)
	}
	plan := resolver.planRunStreamer(loaded, "fixture-key")
	if len(plan.skipped) != 0 || len(plan.targets) != 1 {
		t.Fatalf("plan skipped %+v, targets %d", plan.skipped, len(plan.targets))
	}
	if target := plan.targets[0].config.Model; target.APIKeyEnv != "FIXTURE_KEY" || requestTemperature(t, target.Request) != "0.2" {
		t.Fatalf("fallback target = %s %s", target.APIKeyEnv, target.Request)
	}
	if keys := runModelKeys(loaded); strings.Join(keys, ",") != "FIXTURE_KEY" {
		t.Fatalf("hidden keys = %v", keys)
	}
	if key := (boundRunner{config: pinned}).hookAPIKeyEnv(); key != "FIXTURE_KEY" {
		t.Fatalf("hook key = %s", key)
	}
	child, err := childConfig(RunTemplate{Base: loaded, Agent: sessionfs.NativeSubagentAgent{Name: "helper"}})
	if err != nil || child.Model.APIKeyEnv != "FIXTURE_KEY" || requestTemperature(t, child.Model.Request) != "0.2" {
		t.Fatalf("sub-agent model = %+v, %v", child.Model, err)
	}

	// Without the copy each of them reads the edit: the fallback is left out and the keys move.
	if _, err := config.WithModel(); err == nil || !strings.Contains(err.Error(), "load the session again") {
		t.Fatalf("unpinned model = %v", err)
	}
	stale := config
	stale.Model = loaded.Model
	if plan := resolver.planRunStreamer(stale, "fixture-key"); len(plan.targets) != 0 || len(plan.skipped) != 1 {
		t.Fatalf("unpinned plan = %d targets, %+v", len(plan.targets), plan.skipped)
	}
}

// A remote sub-agent run reads the own models once when it starts; its child loads its model from
// that copy, so an edit after the start does not reach it.
func TestARemoteSubAgentUsesTheCopyItsStartTook(t *testing.T) {
	service, w := pinWorkspace(t, &recordingProvider{}, `{}`)
	resolved, err := service.resolver.Resolve(context.Background(), "s1", "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	base, err := remoteSubagentBase(resolved.Runner.(boundRunner).config)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, pinOwnModel(w, "fast"), pinOwnModelFile("global:openai/other", "0.9"))
	child, err := childConfig(RunTemplate{Base: base, Agent: sessionfs.NativeSubagentAgent{Name: "helper"}})
	if err != nil || child.Model.APIKeyEnv != "FIXTURE_KEY" || requestTemperature(t, child.Model.Request) != "0.2" {
		t.Fatalf("remote sub-agent model = %+v, %v", child.Model, err)
	}
}
