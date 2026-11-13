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
	return filepath.Join(fixture.GlobalDir, "models", filepath.FromSlash(id)+".json")
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

// A model the profile names as its model_default is hidden like any other that fails, and it
// is never replaced by another: the session has no default model, loads (so it can be listed and
// read), and a run that names no model stops with the file's error.
func TestAModelDefaultThatFailsIsHiddenAndNotReplaced(t *testing.T) {
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
	if want := []string{"m/b"}; !reflect.DeepEqual(config.Models, want) || config.ModelDefault != "" || config.ModelPath != "" {
		t.Errorf("Models = %v default %q path %q, want only m/b, no default and no model file", config.Models, config.ModelDefault, config.ModelPath)
	}
	if _, listed := config.ModelPaths["m/a"]; listed {
		t.Errorf("ModelPaths = %v lists the hidden default", config.ModelPaths)
	}
	if config.ModelFault == nil || !strings.Contains(config.ModelFault.Error(), modelFileOf(fixture, "m/a")) {
		t.Fatalf("ModelFault = %v, want the error of m/a's file", config.ModelFault)
	}
	if !hasWarning(config, "model m/a left out", "the profile's model_default") || !hasWarning(config, "model m/c left out") {
		t.Errorf("Warnings = %q", config.Warnings)
	}
	if _, err := config.WithModel(); err == nil || !strings.Contains(err.Error(), modelFileOf(fixture, "m/a")) {
		t.Fatalf("a run with no model named: error = %v, want the default's file error", err)
	}
	// Reading the session does not need the model: it loads, and nothing is read in its place.
	read, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if read.Model.Name != "" || read.ModelFault == nil {
		t.Errorf("LoadSessionConfig used a model: %+v fault %v", read.Model, read.ModelFault)
	}
	// A model of the list that passes can still be asked for by name.
	chosen, err := LoadSessionConfig(location, "", "m/b")
	if err != nil || chosen.Model.Name != "beta-model" || chosen.ModelFault != nil || chosen.ModelPath != modelFileOf(fixture, "m/b") {
		t.Fatalf("requesting m/b: %v %+v", err, chosen.ModelFault)
	}
	// Repaired, it is offered and the default again.
	writeFileT(t, modelFileOf(fixture, "m/a"), `{"provider":"alpha","model":"alpha-model","api_key_env":"FIXTURE_KEY"}`)
	config, err = LoadSessionSettings(location, "", "")
	if err != nil || config.ModelDefault != "m/a" || config.ModelFault != nil || !reflect.DeepEqual(config.Models, []string{"m/a", "m/b"}) {
		t.Fatalf("after the repair: %v default %q fault %v Models %v", err, config.ModelDefault, config.ModelFault, config.Models)
	}
}

// A model_default with an effort on a model whose file fails does not hide the profile.
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
	if config.Profile != "dev/coder" || config.ModelDefault != "" || config.ReasoningEffort != "" || config.ModelFault == nil {
		t.Errorf("Profile %q default %q effort %q fault %v", config.Profile, config.ModelDefault, config.ReasoningEffort, config.ModelFault)
	}
}

// A model the caller requests that fails is not offered, so it is not loaded in its place or
// replaced: the load succeeds with no model, the explicit load of it and a run say which file.
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
	if config.ModelPath != "" || config.ReasoningEffort != "" || config.ModelDefault != "m/a" {
		t.Fatalf("ModelPath = %q effort %q default %q, want no model file and the profile's own default kept", config.ModelPath, config.ReasoningEffort, config.ModelDefault)
	}
	if _, listed := config.ModelPaths["m/c"]; listed || !reflect.DeepEqual(config.Models, []string{"m/a", "m/b"}) {
		t.Errorf("m/c is still offered: Models %v ModelPaths %v", config.Models, config.ModelPaths)
	}
	if _, err := config.WithModel(); err == nil || !strings.Contains(err.Error(), modelFileOf(fixture, "m/c")) {
		t.Fatalf("WithModel error = %v, want the requested file's error", err)
	}
	if _, err := LoadSessionConfig(location, "", "m/c@high"); err == nil || !strings.Contains(err.Error(), modelFileOf(fixture, "m/c")) {
		t.Fatalf("an explicit load of m/c@high: error = %v, want its file's error", err)
	}
	// Without a request the session uses its own default as always.
	if plain, err := LoadSessionConfig(location, "", ""); err != nil || plain.Model.Name != "alpha-model" {
		t.Fatalf("the default: %v %+v", err, plain.Model)
	}
	// A model the profile does not list is still "not available", whatever its file.
	if _, err := LoadSessionSettings(location, "", "m/zzz"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("an unlisted model: error = %v", err)
	}
}

// When no model of the profile passes, none is offered. The session still loads, and a run
// reports each file; nothing is run in their place.
func TestEveryModelFailingOffersNoneAndTheSessionStillLoads(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"no default named":      {},
		"an explicit default":   {"model_default": "m/b"},
		"a default with effort": {"model_default": "m/b@high"},
	} {
		t.Run(name, func(t *testing.T) {
			profile := map[string]any{"models": []string{"m/a", "m/b"}}
			for key, value := range extra {
				profile[key] = value
			}
			location, fixture := layoutLocation(t, layouttest.Spec{Models: isolationModels(), Profiles: map[string]map[string]any{"dev/coder": profile}})
			corrupt(t, modelFileOf(fixture, "m/a"))
			corrupt(t, modelFileOf(fixture, "m/b"))
			config, err := LoadSessionSettings(location, "", "")
			if err != nil {
				t.Fatalf("settings: %v", err)
			}
			if config.Models == nil || len(config.Models) != 0 || len(config.ModelPaths) != 0 || config.ModelDefault != "" || config.ModelPath != "" {
				t.Errorf("Models = %#v ModelPaths %v default %q path %q, want an empty offer", config.Models, config.ModelPaths, config.ModelDefault, config.ModelPath)
			}
			if !hasWarning(config, "model m/a left out") || !hasWarning(config, "model m/b left out") || !hasWarning(config, "no model of profile dev/coder can be used") {
				t.Errorf("Warnings = %q", config.Warnings)
			}
			if _, err := config.WithModel(); err == nil || !strings.Contains(err.Error(), modelFileOf(fixture, "m/b")) {
				t.Fatalf("WithModel error = %v, want the file's error", err)
			}
			if name == "no default named" {
				if _, err := config.WithModel(); !strings.Contains(err.Error(), modelFileOf(fixture, "m/a")) {
					t.Errorf("the error names only some of the files: %v", err)
				}
			}
			if read, err := LoadSessionConfig(location, "", ""); err != nil || read.ModelFault == nil {
				t.Fatalf("LoadSessionConfig: %v fault %v", err, read.ModelFault)
			}
			// A requested one is the explicit load that fails.
			if _, err := LoadSessionConfig(location, "", "m/a"); err == nil || !strings.Contains(err.Error(), modelFileOf(fixture, "m/a")) {
				t.Fatalf("requesting m/a: error = %v", err)
			}
		})
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
	writeFileT(t, filepath.Join(fixture.GlobalDir, "tools", "coding", "default.json"), `{"bogus":1}`)
	config, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if config.Profile != "dev/bbb" || !hasWarning(config, "profile dev/aaa left out", "bogus") {
		t.Errorf("Profile %q Warnings %q", config.Profile, config.Warnings)
	}
}

// A toolset that parses but fails when it is applied to the profile (an MCP server that does
// not exist, a malformed mcp object, hooks given both in the profile's settings and the
// toolset) hides the profile as a whole, as an unparsable toolset does: the profile list, the
// default and the profile info agree, the named profile is an error with the toolset's file,
// and a repair brings it back.
func TestAProfileWhoseToolsetFailsWhenAppliedIsLeftOut(t *testing.T) {
	cases := map[string]struct {
		toolset string
		profile map[string]any
		want    string
	}{
		"an mcp server that does not exist": {`{"mcp":{"nope/missing":{}}}`, map[string]any{}, "nope/missing"},
		"an mcp that is not an object":      {`{"mcp":[1]}`, map[string]any{}, "mcp must be an object"},
		"hooks in the settings and toolset": {`{"hooks":{"pre_tool":[]}}`, map[string]any{"settings": map[string]any{"hooks": map[string]any{"pre_tool": []any{}}}}, "hooks is set here and in the settings"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			profile := map[string]any{"models": []string{"m/a"}, "toolset": "coding/default"}
			for key, value := range test.profile {
				profile[key] = value
			}
			location, fixture := layoutLocation(t, layouttest.Spec{
				Models:   isolationModels(),
				Native:   map[string]any{},
				Profiles: map[string]map[string]any{"dev/aaa": profile, "dev/bbb": {"models": []string{"m/b"}}},
			})
			toolsetFile := filepath.Join(fixture.GlobalDir, "tools", "coding", "default.json")
			writeFileT(t, toolsetFile, test.toolset)

			config, err := LoadSessionSettings(location, "", "")
			if err != nil {
				t.Fatalf("a toolset that fails to apply failed the session: %v", err)
			}
			if config.Profile != "dev/bbb" || !reflect.DeepEqual(config.Profiles, []string{"dev/bbb"}) || config.DefaultProfile != "dev/bbb" {
				t.Errorf("Profile %q Profiles %v default %q", config.Profile, config.Profiles, config.DefaultProfile)
			}
			if !hasWarning(config, "profile dev/aaa left out", toolsetFile, test.want, "the default profile is dev/bbb instead") {
				t.Errorf("Warnings = %q", config.Warnings)
			}
			info, err := ReadLayoutProfileInfo(location)
			if err != nil || info.ActiveProfile != "dev/bbb" || !reflect.DeepEqual(info.Profiles, []string{"dev/bbb"}) {
				t.Errorf("ProfileInfo = %+v, %v", info, err)
			}
			if _, err := SetLayoutModelDefault(location, "dev/aaa", "m/a", "none"); err == nil || !strings.Contains(err.Error(), toolsetFile) {
				t.Errorf("setting the model of the left-out profile: error = %v", err)
			}
			// Named, it is an error with the toolset's file and nothing else takes its place.
			if _, err := LoadSessionSettings(location, "dev/aaa", ""); err == nil || !strings.Contains(err.Error(), toolsetFile) {
				t.Fatalf("selecting dev/aaa: error = %v", err)
			}

			writeFileT(t, toolsetFile, `{}`)
			config, err = LoadSessionSettings(location, "", "")
			if err != nil || config.Profile != "dev/aaa" || !reflect.DeepEqual(config.Profiles, []string{"dev/aaa", "dev/bbb"}) {
				t.Fatalf("after the repair: %v Profile %q Profiles %v", err, config.Profile, config.Profiles)
			}
		})
	}
}

// A native tool setting of a toolset that fails its own rules (a key the tool does not have)
// leaves the profile out like any other toolset that cannot be applied: the profile lists,
// default and info views agree, the default moves with a warning that names the toolset, a
// named profile is an error, and a repaired toolset brings it back.
func TestAToolsetNativeSettingErrorLeavesTheProfileOut(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Native:   map[string]any{},
		Profiles: map[string]map[string]any{"dev/aaa": {"models": []string{"m/a"}, "toolset": "coding/default"}, "dev/bbb": {"models": []string{"m/b"}}},
	})
	toolsetFile := filepath.Join(fixture.GlobalDir, "tools", "coding", "default.json")
	writeFileT(t, toolsetFile, `{"native":{"exec":{"no_such_setting":1}}}`)

	config, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("a toolset with a bad native setting failed the session: %v", err)
	}
	if config.Profile != "dev/bbb" || !reflect.DeepEqual(config.Profiles, []string{"dev/bbb"}) || config.DefaultProfile != "dev/bbb" {
		t.Errorf("Profile %q Profiles %v default %q", config.Profile, config.Profiles, config.DefaultProfile)
	}
	if !hasWarning(config, "profile dev/aaa left out", "no_such_setting", "toolsets it names (coding/default)", "the default profile is dev/bbb instead") {
		t.Errorf("Warnings = %q", config.Warnings)
	}
	info, err := ReadLayoutProfileInfo(location)
	if err != nil || info.ActiveProfile != "dev/bbb" || !reflect.DeepEqual(info.Profiles, []string{"dev/bbb"}) {
		t.Errorf("ProfileInfo = %+v, %v", info, err)
	}
	// Named, it is an error that names the toolset, and nothing else takes its place.
	if _, err := LoadSessionSettings(location, "dev/aaa", ""); err == nil || !strings.Contains(err.Error(), "no_such_setting") {
		t.Fatalf("selecting dev/aaa: error = %v", err)
	}
	// The profile that passes, selected by name, does not list the one that is left out.
	if config, err = LoadSessionSettings(location, "dev/bbb", ""); err != nil || !reflect.DeepEqual(config.Profiles, []string{"dev/bbb"}) {
		t.Fatalf("dev/bbb named: %v, Profiles %v", err, config.Profiles)
	}

	writeFileT(t, toolsetFile, `{"native":{"exec":{"timeout_ms":1000}}}`)
	config, err = LoadSessionSettings(location, "", "")
	if err != nil || config.Profile != "dev/aaa" || !reflect.DeepEqual(config.Profiles, []string{"dev/aaa", "dev/bbb"}) {
		t.Fatalf("after the repair: %v Profile %q Profiles %v", err, config.Profile, config.Profiles)
	}
}

// Only the profile that would be the default is judged: a profile that is not stays offered, and
// choosing it fails with the setting's error and nothing takes its place. When the default and
// the next one both fail through the toolset, the default moves on to the first that loads; when
// every profile fails the load fails with the first one's error.
func TestToolsetNativeSettingErrorsAreJudgedForTheDefaultChainOnly(t *testing.T) {
	bad := `{"native":{"exec":{"no_such_setting":1}}}`
	profile := func(toolset string) map[string]any {
		file := map[string]any{"models": []string{"m/a"}}
		if toolset != "" {
			file["toolset"] = toolset
		}
		return file
	}
	t.Run("a profile that is not the default stays offered", func(t *testing.T) {
		location, fixture := layoutLocation(t, layouttest.Spec{Models: isolationModels(), Native: map[string]any{}, Profiles: map[string]map[string]any{"dev/aaa": profile(""), "dev/bbb": profile("coding/default")}})
		writeFileT(t, filepath.Join(fixture.GlobalDir, "tools", "coding", "default.json"), bad)
		config, err := LoadSessionSettings(location, "", "")
		if err != nil || config.Profile != "dev/aaa" || !reflect.DeepEqual(config.Profiles, []string{"dev/aaa", "dev/bbb"}) {
			t.Fatalf("default load: %v Profile %q Profiles %v", err, config.Profile, config.Profiles)
		}
		if _, err := LoadSessionSettings(location, "dev/bbb", ""); err == nil || !strings.Contains(err.Error(), "no_such_setting") {
			t.Fatalf("dev/bbb named: error = %v", err)
		}
	})
	t.Run("the default moves past every profile the toolset fails", func(t *testing.T) {
		location, fixture := layoutLocation(t, layouttest.Spec{Models: isolationModels(), Native: map[string]any{}, Profiles: map[string]map[string]any{"dev/aaa": profile("coding/default"), "dev/bbb": profile("coding/default"), "dev/ccc": profile("")}})
		writeFileT(t, filepath.Join(fixture.GlobalDir, "tools", "coding", "default.json"), bad)
		config, err := LoadSessionSettings(location, "", "")
		if err != nil || config.Profile != "dev/ccc" || !reflect.DeepEqual(config.Profiles, []string{"dev/ccc"}) {
			t.Fatalf("load: %v Profile %q Profiles %v", err, config.Profile, config.Profiles)
		}
		if !hasWarning(config, "profile dev/aaa left out", "the default profile is dev/ccc instead") || !hasWarning(config, "profile dev/bbb left out") {
			t.Errorf("Warnings = %q", config.Warnings)
		}
	})
	t.Run("every profile failing fails the load", func(t *testing.T) {
		location, fixture := layoutLocation(t, layouttest.Spec{Models: isolationModels(), Native: map[string]any{}, Profiles: map[string]map[string]any{"dev/aaa": profile("coding/default"), "dev/bbb": profile("coding/default")}})
		writeFileT(t, filepath.Join(fixture.GlobalDir, "tools", "coding", "default.json"), bad)
		if _, err := LoadSessionSettings(location, "", ""); err == nil || !strings.Contains(err.Error(), "no_such_setting") {
			t.Fatalf("error = %v", err)
		}
	})
}

// Only a failure that the toolsets cause leaves a profile out. A setting the profile itself or
// the session carries that fails its rules still fails the load, with a good toolset or a bad
// one, and no other profile takes the place of the one that fails.
func TestASettingThatIsNotTheToolsetsStillFailsTheLoad(t *testing.T) {
	tests := map[string]struct {
		profile map[string]any
		session map[string]any
	}{
		"the profile's own setting": {
			profile: map[string]any{"models": []string{"m/a"}, "toolset": "coding/default", "settings": map[string]any{"max_tool_rounds": "many"}},
		},
		"the profile's own setting with a good toolset": {
			profile: map[string]any{"models": []string{"m/a"}, "toolset": "coding/good", "settings": map[string]any{"max_tool_rounds": "many"}},
		},
		"the session's setting": {
			profile: map[string]any{"models": []string{"m/a"}, "toolset": "coding/default"},
			session: map[string]any{"max_tool_rounds": "many"},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			spec := layouttest.Spec{
				Models:   isolationModels(),
				Native:   map[string]any{},
				Profiles: map[string]map[string]any{"dev/aaa": test.profile, "dev/bbb": {"models": []string{"m/b"}}},
			}
			if test.session != nil {
				spec.Settings = test.session
			}
			location, fixture := layoutLocation(t, spec)
			writeFileT(t, filepath.Join(fixture.GlobalDir, "tools", "coding", "default.json"), `{"native":{"exec":{"no_such_setting":1}}}`)
			writeFileT(t, filepath.Join(fixture.GlobalDir, "tools", "coding", "good.json"), `{"native":{}}`)
			config, err := LoadSessionSettings(location, "", "")
			if err == nil || !strings.Contains(err.Error(), "max_tool_rounds") {
				t.Fatalf("the load = Profile %q, %v; want the setting's error", config.Profile, err)
			}
		})
	}
}

// When every profile of the scope fails to apply its toolset, the session fails with the first
// profile's error.
func TestEveryProfileToolsetFailingToApplyFailsTheLoad(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		Models:   isolationModels(),
		Native:   map[string]any{},
		Profiles: map[string]map[string]any{"dev/aaa": {"models": []string{"m/a"}, "toolset": "coding/default"}, "dev/bbb": {"models": []string{"m/b"}, "toolset": "coding/default"}},
	})
	toolsetFile := filepath.Join(fixture.GlobalDir, "tools", "coding", "default.json")
	writeFileT(t, toolsetFile, `{"mcp":{"nope/missing":{}}}`)
	if _, err := LoadSessionSettings(location, "", ""); err == nil || !strings.Contains(err.Error(), toolsetFile) {
		t.Fatalf("error = %v, want the toolset's file", err)
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
			backdateTree(t, fixture.Root, fixture.GlobalDir)
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
	layout := Layout{Models: filepath.Join(fixture.GlobalDir, "models")}
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
