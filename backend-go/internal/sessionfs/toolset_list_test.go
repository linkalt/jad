package sessionfs

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func profileSpec(toolset any) layouttest.Spec {
	return layouttest.Spec{Profiles: map[string]map[string]any{"dev/coder": {
		"models": []string{"openai/default"}, "model_default": "openai/default", "toolset": toolset,
	}}}
}

const (
	grepToolsetText = `{"native":{"read_image":{}}}`
	globToolsetText = `{"native":{"skill":{}}}`
)

func TestProfileToolsetAcceptsAStringOrAnArray(t *testing.T) {
	files := map[string]string{"a/one": grepToolsetText, "b/two": globToolsetText}
	for name, tc := range map[string]struct {
		toolset  any
		wantGrep bool
		wantGlob bool
	}{
		"string":       {toolset: "a/one", wantGrep: true},
		"one element":  {toolset: []string{"a/one"}, wantGrep: true},
		"two":          {toolset: []string{"a/one", "b/two"}, wantGrep: true, wantGlob: true},
		"empty string": {toolset: ""},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := loadToolsetSession(t, profileSpec(tc.toolset), files)
			if err != nil {
				t.Fatalf("LoadSessionSettings: %v", err)
			}
			if (config.NativeReadImage != nil) != tc.wantGrep || (config.NativeSkill != nil) != tc.wantGlob {
				t.Errorf("read_image %v skill %v, want read_image %v skill %v", config.NativeReadImage != nil, config.NativeSkill != nil, tc.wantGrep, tc.wantGlob)
			}
		})
	}
}

func TestProfileToolsetRefusesABadList(t *testing.T) {
	files := map[string]string{"a/one": grepToolsetText, "b/two": globToolsetText}
	for name, tc := range map[string]struct {
		toolset any
		want    string
	}{
		"empty array":      {toolset: []string{}, want: "toolset must be a non-empty string or an array"},
		"blank element":    {toolset: []string{"a/one", " "}, want: "toolset must be a non-empty string or an array"},
		"duplicate id":     {toolset: []string{"a/one", "a/one"}, want: "toolset lists a/one twice"},
		"number":           {toolset: 3, want: "toolset must be a non-empty string or an array"},
		"object":           {toolset: map[string]any{"a": 1}, want: "toolset must be a non-empty string or an array"},
		"number in array":  {toolset: []any{"a/one", 2}, want: "toolset must be a non-empty string or an array"},
		"unknown id":       {toolset: []string{"a/one", "b/missing"}, want: "b/missing"},
		"bare id in array": {toolset: []string{"a/one", "two"}, want: `toolset "two" must be written <category>/<name>`},
		"padded id":        {toolset: " a/one ", want: "a/one"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadToolsetSession(t, profileSpec(tc.toolset), files)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestSubagentToolsetAcceptsAStringOrAnArrayAndBuildsItsKey(t *testing.T) {
	files := map[string]string{"a/one": grepToolsetText, "b/two": globToolsetText}
	for name, tc := range map[string]struct {
		toolset any
		want    []string
		key     string
	}{
		"string":      {toolset: "a/one", want: []string{"a/one"}, key: "a/one"},
		"one element": {toolset: []string{"a/one"}, want: []string{"a/one"}, key: "a/one"},
		"two":         {toolset: []string{"a/one", "b/two"}, want: []string{"a/one", "b/two"}, key: "a/one+b/two"},
		"padded":      {toolset: []string{" a/one ", "b/two"}, want: []string{"a/one", "b/two"}, key: "a/one+b/two"},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := loadToolsetSession(t, toolsetSpec(map[string]any{"toolset": tc.toolset}), files)
			if err != nil {
				t.Fatalf("LoadSessionSettings: %v", err)
			}
			agent, ok := config.NativeSubagent.Agent("finder")
			if !ok || !reflect.DeepEqual(agent.Toolset, tc.want) || agent.ToolsetKey() != tc.key {
				t.Fatalf("agent toolset = %v key %q, want %v key %q", agent.Toolset, agent.ToolsetKey(), tc.want, tc.key)
			}
		})
	}
}

func TestSubagentToolsetRefusesABadList(t *testing.T) {
	files := map[string]string{"a/one": grepToolsetText}
	for name, tc := range map[string]struct {
		toolset any
		want    string
	}{
		"empty array":   {toolset: []string{}, want: "toolset must be a non-empty string or an array"},
		"blank element": {toolset: []string{"a/one", ""}, want: "toolset must be a non-empty string or an array"},
		"duplicate id":  {toolset: []string{"a/one", "a/one"}, want: "toolset lists a/one twice"},
		"number":        {toolset: 3, want: "toolset must be a non-empty string or an array"},
		"object":        {toolset: map[string]any{}, want: "toolset must be a non-empty string or an array"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadToolsetSession(t, toolsetSpec(map[string]any{"toolset": tc.toolset}), files)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateProfileFileChecksEveryToolsetOfTheList(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{})
	writeToolsetFile(t, fixture, "a/one", grepToolsetText)
	layout := Layout{Models: fixture.ModelsDir, Tools: filepath.Join(fixture.Root, "tools")}
	path := filepath.Join(fixture.Root, "profiles", "dev", "coder.json")
	body := func(toolset string) []byte {
		return []byte(`{"models":["openai/default"],"model_default":"openai/default","toolset":` + toolset + `}`)
	}
	if err := validateProfileFile(layout, path, body(`["a/one"]`)); err != nil {
		t.Fatalf("known toolset: %v", err)
	}
	err := validateProfileFile(layout, path, body(`["a/one","b/missing"]`))
	if err == nil || !strings.Contains(err.Error(), "b/missing") {
		t.Fatalf("error = %v, want it to name b/missing", err)
	}
}

func TestToolsetUsersCountsEveryToolsetOfAProfileList(t *testing.T) {
	fixture := layouttest.Write(t, profileSpec([]string{"a/one", "b/two"}))
	layout := Layout{Profiles: filepath.Join(fixture.Root, "profiles")}
	for id, want := range map[string]int{"a/one": 1, "b/two": 1, "c/none": 0} {
		users, err := toolsetUsers(layout, id, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != want {
			t.Errorf("toolsetUsers(%s) = %v, want %d", id, users, want)
		}
	}
}
