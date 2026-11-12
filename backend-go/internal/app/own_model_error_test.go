package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// ownModelErrorWorkspace is a workspace whose profile dev/target runs with own models: its
// default, openai/broken, names a base that does not exist, and openai/tuned offers the effort
// low only. dev/coder uses the shared model.
func ownModelErrorWorkspace(t *testing.T) (*Service, string) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	w := filepath.Join(base, "w")
	put := func(path, text string) { writeWorkspaceTestFile(t, path, text) }
	put(filepath.Join(home, ".jad", "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-base","api_key_env":"FIXTURE_KEY"}`)
	put(filepath.Join(w, ".jad", "workspace.json"), `{}`)
	put(filepath.Join(w, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
	put(filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"]}`)
	put(filepath.Join(w, ".jad", "profiles", "dev", "target.json"), `{"models":["openai/default","openai/broken","openai/tuned"],"model_default":"openai/broken"}`)
	own := filepath.Join(w, ".jad", "profile-own", "dev", "target", "models", "openai")
	put(filepath.Join(own, "broken.json"), `{"based_on":"global:openai/missing"}`)
	put(filepath.Join(own, "tuned.json"), `{"based_on":"global:openai/default","set":{"efforts":["low"]}}`)
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: w, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken, "HOME": home}})
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

// An error about one of a profile's own model files names a file under profile-own and why it
// fails. Another profile reads only that the selected model could not be loaded, whether it
// messages the profile or compacts it; a model it names that is one of the target's own is
// refused before any run, with no text of the own file. The profile itself keeps the detail
// (decider 3c.2a).
func TestOwnModelErrorsReachAnotherProfileWithoutTheirDetail(t *testing.T) {
	service, w := ownModelErrorWorkspace(t)
	resolver := service.resolver
	messenger := profileMessenger{resolver: resolver}
	background := context.Background()
	callerRun, err := resolver.Resolve(background, "s1", "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(background, callerMessagesPathKey{}, callerRun.Runner.(boundRunner).config.MessagesPath)
	const owner = "s1\x00dev/coder"
	hidden := func(what, text string) {
		t.Helper()
		for _, detail := range []string{"profile-own", "broken.json", "tuned.json", "missing", "gpt-base", "available"} {
			if strings.Contains(text, detail) {
				t.Fatalf("%s tells %q: %s", what, detail, text)
			}
		}
	}

	// The profile's own session keeps the whole text.
	location, err := sessionfs.Resolve(resolver.roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	own, err := sessionfs.LoadSessionSettings(location, "dev/target", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sessionfs.IsOwnModelError(own.ModelFault) || !strings.Contains(own.ModelFault.Error(), "broken.json") {
		t.Fatalf("the target's own fault = %v", own.ModelFault)
	}
	if _, err := own.WithModel(); !sessionfs.IsOwnModelError(err) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("the target's own run = %v", err)
	}

	// A model the call names that is one of the target's own is refused before any run, the
	// same way whether it is usable, offers no such effort or cannot be loaded.
	for _, model := range []string{"openai/tuned", "openai/tuned@high", "openai/broken", "openai/broken@low"} {
		_, err := messenger.send(ctx, native.ProfileMessageRequest{OwnerKey: owner, Target: "dev/target", Message: "hi", Model: model})
		var modelErr *native.ProfileModelError
		if !errors.As(err, &modelErr) || modelErr.Model != model || !strings.Contains(modelErr.Message, "own models") {
			t.Fatalf("model %s: err = %v", model, err)
		}
		hidden("model "+model, modelErr.Message)
	}
	if transcriptOf(t, w, "dev/target") != "" {
		t.Fatal("a refused own model still started a run of the target")
	}

	// The target's own default cannot be loaded: its run fails, and the caller reads no detail.
	_, err = messenger.send(ctx, native.ProfileMessageRequest{OwnerKey: owner, Target: "dev/target", Message: "hi"})
	var failed *native.ProfileFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("a message to the target = %v", err)
	}
	hidden("the failed run", failed.Message)
	if transcriptOf(t, w, "dev/target") == "" {
		t.Fatal("the failed run left no transcript, so the check above proves nothing")
	}

	// compact of another profile: only that the selected model could not be loaded.
	err = messenger.CompactProfile(background, owner, "s1", "dev/target")
	if err == nil || err.Error() != "profile dev/target: its selected model could not be loaded" {
		t.Fatalf("compact from another profile = %v", err)
	}
	// The profile itself, from its own session, reads the whole text.
	if err := messenger.CompactProfile(background, "s1\x00dev/target", "s1", "dev/target"); err == nil || !strings.Contains(err.Error(), "broken.json") {
		t.Fatalf("compact from the profile itself = %v", err)
	}
}

// transcriptOf is the target profile's transcript in session s1 of workspace w, "" when there is
// none.
func transcriptOf(t *testing.T, w, profile string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(filepath.Join(w, ".jad"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == "messages.jsonl" && strings.Contains(filepath.ToSlash(path), "/"+profile+"/") {
			data, _ := os.ReadFile(path)
			found += string(data)
		}
		return nil
	})
	return found
}

// otherProfileError swaps only an own model error, and only for a caller of another profile.
func TestOtherProfileErrorSwapsOnlyAnOwnModelErrorForAnotherProfile(t *testing.T) {
	ownErr := &sessionfs.OwnModelError{Err: &sessionfs.ConfigError{Message: "/w/.jad/profile-own/dev/target/models/openai/broken.json: based_on: no such model"}}
	wrapped := mapSessionError(ownErr)
	if !sessionfs.IsOwnModelError(wrapped) {
		t.Fatalf("the status lost the mark: %#v", wrapped)
	}
	if got := otherProfileError("s1\x00dev/coder", "dev/target", wrapped); got == nil || got.Error() != "profile dev/target: its selected model could not be loaded" {
		t.Fatalf("another profile = %v", got)
	}
	if got := otherProfileError("s2\x00dev/target", "dev/target", wrapped); got != wrapped {
		t.Fatalf("the profile itself = %v", got)
	}
	if got := otherProfileError("s1", "dev/target", wrapped); got == wrapped {
		t.Fatal("a caller without a profile read the detail")
	}
	plain := errors.New("busy")
	if got := otherProfileError("s1\x00dev/coder", "dev/target", plain); got != plain {
		t.Fatalf("another error = %v", got)
	}
}
