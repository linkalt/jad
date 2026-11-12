package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// ownToolsetWorkspace is a workspace in base whose profile dev/coder uses its own toolset
// coding/lean, based on the global coding/full (async and skill); dev/other uses the shared
// model only. sessionSettings are the session file's settings.
func ownToolsetWorkspace(t *testing.T, base string, provider *recordingProvider, sessionSettings, lean string) (*Service, string) {
	t.Helper()
	server := httptest.NewServer(provider.handler())
	t.Cleanup(server.Close)
	home := filepath.Join(base, "home")
	w := filepath.Join(base, "w")
	put := func(path, text string) { writeWorkspaceTestFile(t, path, text) }
	put(filepath.Join(home, ".jad", "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY","base_url":"`+server.URL+`/v1"}`)
	put(filepath.Join(home, ".jad", "tools", "coding", "full.json"), `{"schema_version":1,"native":{"async":{},"skill":{}}}`)
	definitions := make([]string, 0, 2)
	for _, name := range []string{"async", "skill"} {
		definition, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, definition)
	}
	libraries, _ := json.Marshal(map[string]any{"libraries": map[string]any{"tool_definition_paths": definitions}})
	put(filepath.Join(home, ".jad", "global.json"), string(libraries))
	put(filepath.Join(w, ".jad", "workspace.json"), `{}`)
	put(filepath.Join(w, ".jad", "sessions", "s1.json"), `{"schema_version":1,"settings":`+sessionSettings+`}`)
	put(filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"toolset":"profile:coding/lean"}`)
	put(filepath.Join(w, ".jad", "profiles", "dev", "other.json"), `{"models":["openai/default"]}`)
	put(ownLeanPath(w), lean)
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: w, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken, "HOME": home, "FIXTURE_KEY": "fixture-key"}})
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

func ownLeanPath(w string) string {
	return filepath.Join(w, ".jad", "profile-own", "dev", "coder", "tools", "coding", "lean.json")
}

// requestToolNames are the names of the tools a provider request offers.
func requestToolNames(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var offered []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &offered); err != nil {
		t.Fatalf("tools %s: %v", raw, err)
	}
	names := make([]string, 0, len(offered))
	for _, tool := range offered {
		names = append(names, tool.Name)
	}
	return names
}

// A prompt_submit command rewrites the run's own toolset after the run started: the run still
// offers the tools of the toolset it read when its session was loaded, and the next run uses the
// edit (decider 3c.2b, for toolsets: a run never reads its own toolset again).
func TestARunKeepsTheOwnToolsetItStartedWith(t *testing.T) {
	provider := &recordingProvider{}
	base := t.TempDir()
	w := filepath.Join(base, "w")
	rewrite := filepath.Join(base, "rewrite.json")
	writeWorkspaceTestFile(t, rewrite, `{"based_on":"global:coding/full","omit":{"native":["skill"]}}`)
	hook := runHookEntry("edit", "cp '"+rewrite+"' '"+ownLeanPath(w)+"'")
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"enabled": true, "prompt_submit": []any{hook}}})
	service, _ := ownToolsetWorkspace(t, base, provider, string(settings), `{"based_on":"global:coding/full","omit":{"native":["async"]}}`)
	run := func() []string {
		t.Helper()
		resolved, err := service.resolver.Resolve(context.Background(), "s1", "dev/coder", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolved.Runner.Run(context.Background(), json.RawMessage(`"hello"`), session.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		requests := provider.requests()
		return requestToolNames(t, requests[len(requests)-1]["tools"])
	}
	if got := run(); !slices.Contains(got, "skill") || slices.Contains(got, "async") {
		t.Fatalf("the run offered %v, not the toolset it started with (skill, no async)", got)
	}
	if got := run(); slices.Contains(got, "skill") || !slices.Contains(got, "async") {
		t.Fatalf("the next run offered %v, want the edit (async, no skill)", got)
	}
}

// An error about one of a profile's own toolset files names a file under profile-own; another
// profile reads only that one of its own toolsets could not be loaded, the profile itself the
// whole text (decider 3c.2a, for toolsets).
func TestOwnToolsetErrorsReachAnotherProfileWithoutTheirDetail(t *testing.T) {
	service, w := ownToolsetWorkspace(t, t.TempDir(), &recordingProvider{}, `{}`, `{"based_on":"global:coding/full","set":{"native":{"async":{"max_handles":9}}}}`)
	messenger := profileMessenger{resolver: service.resolver}
	background := context.Background()
	err := messenger.CompactProfile(background, "s1\x00dev/other", "s1", "dev/coder")
	if err == nil || err.Error() != "profile dev/coder: one of its own toolsets could not be loaded" {
		t.Fatalf("compact from another profile = %v", err)
	}
	if _, err := messenger.ProfileInfo(background, "s1\x00dev/other", "s1", "dev/coder"); err == nil || strings.Contains(err.Error(), "profile-own") || strings.Contains(err.Error(), "max_handles") {
		t.Fatalf("info from another profile = %v", err)
	}
	if err := messenger.CompactProfile(background, "s1\x00dev/coder", "s1", "dev/coder"); err == nil || !strings.Contains(err.Error(), "lean.json") || !strings.Contains(err.Error(), "max_handles") {
		t.Fatalf("compact from the profile itself = %v", err)
	}
	// A profile_message to it fails before any run with no detail either.
	callerRun, err := service.resolver.Resolve(background, "s1", "dev/other", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(background, callerMessagesPathKey{}, callerRun.Runner.(boundRunner).config.MessagesPath)
	// A profile_message to it is refused before any run; the error is none whose text the tool
	// shows (it renders "Profile dev/coder failed"), and no model error.
	_, err = messenger.send(ctx, native.ProfileMessageRequest{OwnerKey: "s1\x00dev/other", Target: "dev/coder", Message: "hi"})
	var modelErr *native.ProfileModelError
	var failed *native.ProfileFailedError
	var remoteFailed *native.RemoteFailedError
	if err == nil || errors.As(err, &modelErr) || errors.As(err, &failed) || errors.As(err, &remoteFailed) {
		t.Fatalf("a message to it = %#v", err)
	}
	if transcriptOf(t, w, "dev/coder") != "" {
		t.Fatal("the refused message started a run")
	}
}
