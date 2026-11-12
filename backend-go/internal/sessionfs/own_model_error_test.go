package sessionfs

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every way one of a profile's own model files fails is an OwnModelError, its text whole: an own
// file whose base cannot be found, one whose base fails the model rules when composed, an
// effort the own file does not offer, a model_default naming that effort, and "no model it lists
// can be used" when an own model is among them. A shared model that fails is not one (3c.2a).
func TestOwnModelFailuresAreMarkedAndSharedOnesAreNot(t *testing.T) {
	settings := func(t *testing.T, o ownModelFixture, profile, model string) (SessionConfig, error) {
		t.Helper()
		location, err := Resolve(wsMustLoad(t, o.f, o.w).Roots, "s1")
		if err != nil {
			t.Fatal(err)
		}
		return LoadSessionSettings(location, profile, model)
	}
	marked := func(t *testing.T, what string, err error, detail string) {
		t.Helper()
		if !IsOwnModelError(err) || !strings.Contains(err.Error(), detail) {
			t.Fatalf("%s = %v, want an own model error that tells %q", what, err, detail)
		}
	}
	const coder = `{"models":["openai/default","profile:openai"]}`

	// The own file's base cannot be found: its fault, and the run's error.
	o := newOwnModelFixture(t, coder)
	wsPut(t, o.own, `{"based_on":"global:openai/missing"}`)
	config, err := settings(t, o, "dev/coder", "openai/fast")
	if err != nil {
		t.Fatal(err)
	}
	marked(t, "a missing base", config.ModelFault, "fast.json")
	_, err = config.WithModel()
	marked(t, "its run", err, "missing")

	// A shared model the profile offers is not marked when it fails the same way.
	config, err = settings(t, o, "dev/other", "")
	if err != nil || config.ModelFault != nil {
		t.Fatalf("dev/other: %v %v", config.ModelFault, err)
	}
	wsPut(t, o.base, `{"provider":"openai","api_key_env":"FIXTURE_KEY"}`)
	config, err = settings(t, o, "dev/other", "")
	if err != nil || config.ModelFault == nil || IsOwnModelError(config.ModelFault) {
		t.Fatalf("a failing shared model: %v %v", config.ModelFault, err)
	}

	// The base fails the model rules once the own file is composed with it.
	o = newOwnModelFixture(t, coder)
	wsPut(t, o.base, `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY","request":"not an object"}`)
	config, err = settings(t, o, "dev/coder", "openai/fast")
	if err != nil {
		t.Fatal(err)
	}
	marked(t, "a failing base", config.ModelFault, "its base global:openai/default")

	// An effort the own file does not offer, named by the caller or by model_default.
	o = newOwnModelFixture(t, coder)
	_, err = settings(t, o, "dev/coder", "openai/fast@medium")
	marked(t, "an effort the own model does not offer", err, "available: low, high")
	// The own file loses the effort after the session loaded with it: the run's load says so.
	config, err = settings(t, o, "dev/coder", "openai/fast@high")
	if err != nil {
		t.Fatal(err)
	}
	wsPut(t, o.own, `{"schema_version":1,"based_on":"global:openai/default","set":{"efforts":["low"]}}`)
	_, err = config.WithModel()
	marked(t, "an effort the own model no longer offers", err, "available: low")
	wsPut(t, o.own, ownFast)
	_, err = settings(t, o, "dev/coder", "openai/default@medium")
	if err == nil || IsOwnModelError(err) {
		t.Fatalf("an effort the shared model does not offer = %v", err)
	}
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default","profile:openai"],"model_default":"openai/fast@medium"}`)
	_, err = settings(t, o, "dev/coder", "")
	marked(t, "a model_default effort the own model does not offer", err, "model_default")
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"settings":{"model_fallback":{"models":["profile:openai/fast@medium"]}}}`)
	_, err = settings(t, o, "dev/coder", "")
	marked(t, "a fallback effort the own model does not offer", err, "model_fallback")

	// Every model the profile lists fails, an own one among them.
	o = newOwnModelFixture(t, `{"models":["profile:openai"]}`)
	wsPut(t, o.own, `{"based_on":"global:openai/missing"}`)
	config, err = settings(t, o, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	marked(t, "no model the profile lists can be used", config.ModelFault, "no model it lists can be used")
}
