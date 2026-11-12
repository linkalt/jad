package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// A profile's own model is listed composed with its base and marked with the profile scope; the
// session version follows an edit of the base; a profile_message caller cannot name it.
func TestOwnModelIsListedFromItsBase(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	own := filepath.Join(dir, "own.json")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(base, `{"provider":"openai","model":"gpt-base","api_key_env":"K"}`)
	write(own, `{"based_on":"global:openai/default","set":{"efforts":["low","high"],"request":{"reasoning":{"effort":"low"}}}}`)
	config := sessionfs.SessionConfig{
		Models:     []string{"openai/default", "openai/fast"},
		ModelPaths: map[string]string{"openai/default": base, "openai/fast": own},
		ModelBases: map[string]sessionfs.ModelBase{own: {BasedOn: "global:openai/default", Path: base}},
		ModelPath:  own,
	}
	options := listingModelOptions(config)
	if len(options) != 2 || options[0].Scope != "" || options[1].Scope != sessionfs.OwnModelScope || options[1].Provider != "openai" || !slices.Equal(options[1].Efforts, []string{"low", "high"}) || options[1].DefaultEffort != "low" {
		t.Fatalf("options = %+v", options)
	}
	if summary := listingModelSummary(sessionfs.Location{SessionID: "s1"}, config); summary.Provider != "openai" || summary.Model != "gpt-base" {
		t.Fatalf("summary = %+v", summary)
	}
	before := sessionVersion(config)
	write(base, `{"provider":"openai","model":"gpt-other-base","api_key_env":"K"}`)
	if sessionVersion(config) == before {
		t.Fatal("an edit of the base does not change the session version")
	}

	var modelErr *native.ProfileModelError
	if err := refuseOwnModel(config, "openai/fast"); !errors.As(err, &modelErr) {
		t.Fatalf("a caller naming the target's own model = %v", err)
	}
	if err := refuseOwnModel(config, ""); err != nil {
		t.Fatalf("the target's own default = %v", err)
	}
	config.ModelPath = base
	if err := refuseOwnModel(config, "openai/default"); err != nil {
		t.Fatalf("a shared model = %v", err)
	}
}
