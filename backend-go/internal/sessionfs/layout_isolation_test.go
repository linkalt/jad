package sessionfs

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// A profile or model file that fails its rules is left out with a warning that names the file,
// and the session loads without it. These tests pin what is left out, what is never replaced,
// and that a repaired file comes back.

func isolationModels() map[string]map[string]any {
	return map[string]map[string]any{
		"m/a": {"provider": "alpha", "model": "alpha-model", "api_key_env": "FIXTURE_KEY"},
		"m/b": {"provider": "beta", "model": "beta-model", "api_key_env": "FIXTURE_KEY"},
		"m/c": {"provider": "gamma", "model": "gamma-model", "api_key_env": "FIXTURE_KEY"},
	}
}

func modelFileOf(fixture layouttest.Fixture, id string) string {
	return filepath.Join(fixture.Root, "models", filepath.FromSlash(id)+".json")
}

func profileFileOf(fixture layouttest.Fixture, id string) string {
	return filepath.Join(fixture.Root, "profiles", filepath.FromSlash(id)+".json")
}

func hasWarning(config SessionConfig, parts ...string) bool {
	for _, warning := range config.Warnings {
		matched := true
		for _, part := range parts {
			matched = matched && strings.Contains(warning, part)
		}
		if matched {
			return true
		}
	}
	return false
}

func corrupt(t *testing.T, path string) {
	t.Helper()
	writeFileT(t, path, "{not json")
}

func TestABrokenModelIsLeftOutOfTheProfileAndTheNextOneIsTheDefault(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Profiles: map[string]map[string]any{"dev/coder": {"models": []string{"m/a", "m/b", "m/c"}}},
	})
	corrupt(t, modelFileOf(fixture, "m/a"))

	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("a broken model that is not chosen failed the load: %v", err)
	}
	if want := []string{"m/b", "m/c"}; !reflect.DeepEqual(config.Models, want) {
		t.Errorf("Models = %v, want %v", config.Models, want)
	}
	if _, listed := config.ModelPaths["m/a"]; listed || config.ModelDefault != "m/b" {
		t.Errorf("ModelPaths = %v, ModelDefault = %q, want m/a gone and m/b the default", config.ModelPaths, config.ModelDefault)
	}
	if !hasWarning(config, "model m/a left out of profile dev/coder", modelFileOf(fixture, "m/a")) {
		t.Errorf("Warnings = %q, want one naming m/a and its file", config.Warnings)
	}
	if config.Model.Name != "beta-model" {
		t.Errorf("the run's model is %q, want beta-model", config.Model.Name)
	}
}

// A model the profile names as its model_default is never replaced by another one: it stays
// offered, the session loads, and a run with it stops with the file's error.
func TestAModelDefaultThatFailsIsNotReplaced(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Profiles: map[string]map[string]any{"dev/coder": {"models": []string{"m/a", "m/b", "m/c"}, "model_default": "m/a"}},
	})
	corrupt(t, modelFileOf(fixture, "m/a"))
	corrupt(t, modelFileOf(fixture, "m/c"))

	config, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if want := []string{"m/a", "m/b"}; !reflect.DeepEqual(config.Models, want) || config.ModelDefault != "m/a" {
		t.Errorf("Models = %v default %q, want %v with m/a the default", config.Models, config.ModelDefault, want)
	}
	if !hasWarning(config, "model m/a", "still offered", "its model_default") || !hasWarning(config, "model m/c left out") {
		t.Errorf("Warnings = %q", config.Warnings)
	}
	_, err = config.WithModel()
	if err == nil || !strings.Contains(err.Error(), modelFileOf(fixture, "m/a")) {
		t.Fatalf("a run with the default model: error = %v, want the file's error", err)
	}
}

// A model_default with an effort on a model whose file fails does not fail the profile.
func TestAModelDefaultEffortOnABrokenModelDoesNotHideTheProfile(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Profiles: map[string]map[string]any{"dev/coder": {"models": []string{"m/a", "m/b"}, "model_default": "m/a@high"}},
	})
	corrupt(t, modelFileOf(fixture, "m/a"))
	config, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if config.Profile != "dev/coder" || config.ModelDefault != "m/a@high" {
		t.Errorf("Profile %q default %q", config.Profile, config.ModelDefault)
	}
}

func TestARequestedModelThatFailsIsNotReplaced(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Profiles: map[string]map[string]any{"dev/coder": {"models": []string{"m/a", "m/b", "m/c"}, "model_default": "m/a"}},
	})
	corrupt(t, modelFileOf(fixture, "m/c"))
	config, err := LoadSessionSettings(location, "", "m/c")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if config.ModelPath != modelFileOf(fixture, "m/c") {
		t.Fatalf("ModelPath = %s, want the requested model's file", config.ModelPath)
	}
	if _, err := config.WithModel(); err == nil || !strings.Contains(err.Error(), modelFileOf(fixture, "m/c")) {
		t.Fatalf("WithModel error = %v, want the requested file's error", err)
	}
	if !hasWarning(config, "model m/c", "the requested model") {
		t.Errorf("Warnings = %q", config.Warnings)
	}
}

// When no model of the profile passes, none is hidden: a run reports the default's own error.
func TestEveryModelFailingLeavesThemOfferedWithWarnings(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Profiles: map[string]map[string]any{"dev/coder": {"models": []string{"m/a", "m/b"}}},
	})
	corrupt(t, modelFileOf(fixture, "m/a"))
	corrupt(t, modelFileOf(fixture, "m/b"))
	config, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if want := []string{"m/a", "m/b"}; !reflect.DeepEqual(config.Models, want) || config.ModelDefault != "m/a" {
		t.Errorf("Models = %v default %q", config.Models, config.ModelDefault)
	}
	if !hasWarning(config, "model m/a", "no model of the profile can") || !hasWarning(config, "model m/b", "no model of the profile can") {
		t.Errorf("Warnings = %q", config.Warnings)
	}
	if _, err := LoadSessionConfig(location, "", ""); err == nil || !strings.Contains(err.Error(), modelFileOf(fixture, "m/a")) {
		t.Fatalf("LoadSessionConfig error = %v", err)
	}
}

func TestAModelThatFailsEveryRuleFileLevelIsLeftOut(t *testing.T) {
	for name, text := range map[string]string{
		"empty":            "",
		"not an object":    "[]",
		"missing model":    `{"provider":"alpha"}`,
		"bad base_url":     `{"model":"x","base_url":""}`,
		"unknown schema":   `{"schema_version":9,"model":"x"}`,
		"efforts wrong":    `{"model":"x","efforts":"high"}`,
		"request not obj":  `{"model":"x","request":"y"}`,
		"removed oauth":    `{"model":"x","oauth_auth_file":"f"}`,
		"instructions key": `{"model":"x","instructions":"y"}`,
	} {
		t.Run(name, func(t *testing.T) {
			location, fixture := layoutLocation(t, layouttest.Spec{
				Models:   isolationModels(),
				Profiles: map[string]map[string]any{"dev/coder": {"models": []string{"m/a", "m/b"}, "model_default": "m/b"}},
			})
			writeFileT(t, modelFileOf(fixture, "m/a"), text)
			config, err := LoadSessionSettings(location, "", "")
			if err != nil {
				t.Fatalf("settings: %v", err)
			}
			if want := []string{"m/b"}; !reflect.DeepEqual(config.Models, want) {
				t.Errorf("Models = %v, want %v", config.Models, want)
			}
			if !hasWarning(config, "model m/a left out", modelFileOf(fixture, "m/a")) {
				t.Errorf("Warnings = %q", config.Warnings)
			}
		})
	}
}

// A profile that fails is left out, the default moves to the next profile with a warning that
// says so, and the profile list, the default and the profile info agree.
func TestABrokenProfileIsLeftOutAndTheDefaultMovesWithAWarning(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models: isolationModels(),
		Profiles: map[string]map[string]any{
			"dev/aaa": {"models": []string{"m/a"}, "colour": "red"},
			"dev/bbb": {"models": []string{"m/b"}, "name": "Second"},
			"dev/ccc": {"models": []string{"m/c"}},
		},
		Session: map[string]any{"scope": map[string]any{"profiles": []string{"dev"}}},
	})
	config, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("a broken profile failed the session: %v", err)
	}
	if config.Profile != "dev/bbb" || config.DefaultProfile != "dev/bbb" {
		t.Errorf("Profile %q DefaultProfile %q, want dev/bbb", config.Profile, config.DefaultProfile)
	}
	if want := []string{"dev/bbb", "dev/ccc"}; !reflect.DeepEqual(config.Profiles, want) {
		t.Errorf("Profiles = %v, want %v", config.Profiles, want)
	}
	if _, named := config.ProfileNames["dev/aaa"]; named || config.ProfileNames["dev/bbb"] != "Second" {
		t.Errorf("ProfileNames = %v", config.ProfileNames)
	}
	if !hasWarning(config, "profile dev/aaa left out", profileFileOf(fixture, "dev/aaa"), "colour", "the default profile is dev/bbb instead") {
		t.Errorf("Warnings = %q", config.Warnings)
	}
	info, err := ReadLayoutProfileInfo(location)
	if err != nil {
		t.Fatal(err)
	}
	if info.ActiveProfile != "dev/bbb" || !reflect.DeepEqual(info.Profiles, config.Profiles) {
		t.Errorf("ProfileInfo = %+v, want it to agree with the load", info)
	}

	// The broken profile asked for by name is an error with its file, not another profile.
	_, err = LoadSessionSettings(location, "dev/aaa", "")
	var configError *ConfigError
	if !errors.As(err, &configError) || !strings.Contains(err.Error(), profileFileOf(fixture, "dev/aaa")) {
		t.Fatalf("selecting the broken profile: error = %v", err)
	}

	// Repaired, it comes back and is the default again.
	writeFileT(t, profileFileOf(fixture, "dev/aaa"), `{"models":["m/a"]}`)
	config, err = LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Profile != "dev/aaa" || !reflect.DeepEqual(config.Profiles, []string{"dev/aaa", "dev/bbb", "dev/ccc"}) || len(config.Warnings) != 0 {
		t.Errorf("after the repair: Profile %q Profiles %v Warnings %q", config.Profile, config.Profiles, config.Warnings)
	}
}

// When every profile of the scope fails there is nothing to run: the load fails with the
// error of the profile that would have been the default, as it always did.
func TestEveryProfileFailingFailsTheLoadWithTheFirstError(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Profiles: map[string]map[string]any{"dev/aaa": {"colour": "red"}, "dev/bbb": {"shape": "round"}},
	})
	_, err := LoadSessionSettings(location, "", "")
	var configError *ConfigError
	if !errors.As(err, &configError) || !strings.Contains(err.Error(), profileFileOf(fixture, "dev/aaa")) || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("error = %v, want the first profile's error", err)
	}
	if _, err := ReadLayoutProfileInfo(location); err != nil {
		t.Fatalf("profile info: %v", err)
	}
}

// What composing the profile finds is not skipped: a profile whose sandbox setting is invalid
// fails the session, it is not replaced by another profile that has no sandbox.
func TestASandboxErrorInTheDefaultProfileStillFailsTheLoad(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models: isolationModels(),
		Profiles: map[string]map[string]any{
			"dev/aaa": {"models": []string{"m/a"}, "settings": map[string]any{"sandbox": map[string]any{"enabled": true, "no_such_key": 1}}},
			"dev/bbb": {"models": []string{"m/b"}},
		},
	})
	_, err := LoadSessionSettings(location, "", "")
	if err == nil || !strings.Contains(err.Error(), "no_such_key") {
		t.Fatalf("error = %v, want the sandbox error of the default profile", err)
	}
	_ = fixture
	// The profile that has no sandbox loads when it is named.
	if _, err := LoadSessionSettings(location, "dev/bbb", ""); err != nil {
		t.Fatalf("dev/bbb: %v", err)
	}
}

// A profile that names a toolset that is missing or does not parse is left out; a toolset the
// profile does not use is not read.
func TestAProfileWithABrokenToolsetIsLeftOut(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Native:   map[string]any{},
		Profiles: map[string]map[string]any{"dev/aaa": {"models": []string{"m/a"}, "toolset": "coding/default"}, "dev/bbb": {"models": []string{"m/b"}}},
	})
	writeFileT(t, filepath.Join(fixture.Root, "tools", "coding", "default.json"), `{"bogus":1}`)
	config, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if config.Profile != "dev/bbb" || !hasWarning(config, "profile dev/aaa left out", "bogus") {
		t.Errorf("Profile %q Warnings %q", config.Profile, config.Warnings)
	}
}

// A model_fallback model whose file fails is left out with a warning, the others stay.
func TestAFallbackModelThatFailsIsLeftOut(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Profiles: map[string]map[string]any{"dev/coder": {"models": []string{"m/a"}, "settings": map[string]any{"model_fallback": map[string]any{"models": []string{"m/b", "m/c"}}}}},
	})
	corrupt(t, modelFileOf(fixture, "m/b"))
	config, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if config.ModelFallback == nil || len(config.ModelFallback.Models) != 1 || config.ModelFallback.Models[0].ID != "m/c" || config.ModelFallback.Models[0].ModelPath != modelFileOf(fixture, "m/c") {
		t.Fatalf("ModelFallback = %+v, want only m/c", config.ModelFallback)
	}
	if !hasWarning(config, "model_fallback.models[0]", "model m/b left out") {
		t.Errorf("Warnings = %q", config.Warnings)
	}
}

// The stamp of a load sees the model files it judged and the profile files it read, so an edit
// in place, and a repair, make a cached list entry stale.
func TestTheStampSeesTheModelFilesJudgedAtLoad(t *testing.T) {
	edits := map[string]func(layouttest.Fixture) string{
		"a listed model":             func(f layouttest.Fixture) string { return modelFileOf(f, "m/b") },
		"the model_default":          func(f layouttest.Fixture) string { return modelFileOf(f, "m/a") },
		"a model of another profile": func(f layouttest.Fixture) string { return modelFileOf(f, "m/c") },
		"another profile":            func(f layouttest.Fixture) string { return profileFileOf(f, "dev/other") },
		"the profile itself":         func(f layouttest.Fixture) string { return profileFileOf(f, "dev/coder") },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			fixture := layouttest.Write(t, layouttest.Spec{
				Models: isolationModels(),
				Profiles: map[string]map[string]any{
					"dev/coder": {"models": []string{"m/a", "m/b"}, "model_default": "m/a"},
					"dev/other": {"models": []string{"m/c"}},
				},
				Session: map[string]any{"scope": map[string]any{"profiles": []string{"dev"}}},
			})
			backdateTree(t, fixture.Root)
			_, stamp := stampedLoad(t, fixture)
			path := edit(fixture)
			writeFileT(t, path, `{"models":["m/a"]}`)
			if stamp.Current() {
				t.Fatalf("the stamp is still current after %s was edited", path)
			}
		})
	}
}

// One file that cannot be read does not hide the others in the listing of its kind.
func TestListCatalogFilesKeepsTheOtherRowsWhenOneFileCannotBeRead(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{Models: isolationModels()})
	layout := Layout{Models: filepath.Join(fixture.Root, "models")}
	replaceWith(t, modelFileOf(fixture, "m/b"), "fifo", 0)
	files, err := ListCatalogFiles(layout, CatalogModels)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	var ids []string
	for _, file := range files {
		ids = append(ids, file.ID)
		switch {
		case file.ID == "m/b" && (file.Error == "" || file.ETag != ""):
			t.Errorf("m/b row = %+v, want an error and no etag", file)
		case file.ID != "m/b" && (file.Error != "" || file.ETag == ""):
			t.Errorf("%s row = %+v, want an etag", file.ID, file)
		}
	}
	if want := []string{"m/a", "m/b", "m/c"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("rows = %v, want %v", ids, want)
	}
}
