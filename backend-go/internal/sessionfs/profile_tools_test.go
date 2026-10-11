package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
)

// loadProfileTools loads a layout session with profiles dev/lean (the given file) and
// dev/full (an empty one); active names the selected one.
func loadProfileTools(t *testing.T, active string, lean map[string]any) (SessionConfig, error) {
	t.Helper()
	return loadLayoutSpec(t, layouttest.Spec{
		ActiveProfile: "dev/" + active,
		Profiles:      map[string]map[string]any{"dev/lean": lean, "dev/full": {}},
	})
}

func TestProfileToolsLoadAndReachTheConfig(t *testing.T) {
	allow := []any{"read", "mcp:docs:search", "mcp:docs:*"}
	config, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": allow}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"read", "mcp:docs:search", "mcp:docs:*"}, Deny: nil}
	if !reflect.DeepEqual(config.ProfileTools, want) {
		t.Fatalf("ProfileTools = %+v, want %+v", config.ProfileTools, want)
	}
}

// deny alone subtracts from everything the session registers: HasAllow stays false.
func TestProfileToolsDenyAloneLoadsWithoutAnAllowList(t *testing.T) {
	config, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"deny": []any{"bash", "mcp:docs:*"}}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := ProfileToolSettings{Restricted: true, HasAllow: false, Deny: []string{"bash", "mcp:docs:*"}}
	if !reflect.DeepEqual(config.ProfileTools, want) {
		t.Fatalf("ProfileTools = %+v, want %+v", config.ProfileTools, want)
	}
}

// allow and deny together are both kept; an empty deny is a restriction that hides nothing.
func TestProfileToolsAllowAndDenyTogether(t *testing.T) {
	config, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read", "bash"}, "deny": []any{"bash"}}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"read", "bash"}, Deny: []string{"bash"}}
	if !reflect.DeepEqual(config.ProfileTools, want) {
		t.Fatalf("ProfileTools = %+v, want %+v", config.ProfileTools, want)
	}
	config, err = loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"deny": []any{}}})
	if err != nil || !config.ProfileTools.Restricted || config.ProfileTools.HasAllow || len(config.ProfileTools.Deny) != 0 {
		t.Fatalf("empty deny: %+v, %v", config.ProfileTools, err)
	}
}

func TestProfileToolsEmptyAllowIsRestrictedAndHidesEverything(t *testing.T) {
	config, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{}}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !config.ProfileTools.Restricted || !config.ProfileTools.HasAllow || len(config.ProfileTools.Allow) != 0 {
		t.Fatalf("ProfileTools = %+v, want restricted with no entries", config.ProfileTools)
	}
}

// A profile without a tools block is not restricted.
func TestProfileToolsAbsentMeansUnrestricted(t *testing.T) {
	config, err := loadProfileTools(t, "lean", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if config.ProfileTools.Restricted || config.ProfileTools.Allow != nil {
		t.Errorf("ProfileTools = %+v, want the zero value", config.ProfileTools)
	}
}

// Only the selected profile's block applies: another profile's restriction is not read.
func TestProfileToolsApplyToTheSelectedProfileOnly(t *testing.T) {
	config, err := loadProfileTools(t, "full", map[string]any{"tools": map[string]any{"allow": []any{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if config.Profile != "dev/full" || config.ProfileTools.Restricted {
		t.Fatalf("profile %q restricted %v, want full and unrestricted", config.Profile, config.ProfileTools.Restricted)
	}
}

func TestProfileToolsShapeErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		tools any
		want  string
	}{
		"not an object":    {[]any{"read"}, "tools must be an object"},
		"null":             {nil, "tools must be an object"},
		"no allow or deny": {map[string]any{}, "tools needs allow or deny; use an empty allow array to hide every tool"},
		"unknown key":      {map[string]any{"allow": []any{}, "block": []any{}}, "tools.block is not supported"},
		"deny not array":   {map[string]any{"deny": "bash"}, "tools.deny must be an array of tool names"},
		"deny null":        {map[string]any{"deny": nil}, "tools.deny must be an array of tool names"},
		"deny not string":  {map[string]any{"deny": []any{3}}, "tools.deny[0] must be a non-empty string"},
		"deny unknown":     {map[string]any{"deny": []any{"raed"}}, `tools.deny[0] "raed" is not a native tool`},
		"deny bad mcp":     {map[string]any{"deny": []any{"mcp:docs"}}, `tools.deny[0] "mcp:docs" must be mcp:<server>:<tool> or mcp:<server>:*`},
		"deny duplicate":   {map[string]any{"deny": []any{"bash", "bash"}}, `tools.deny[1] repeats "bash"`},
		"typo key":         {map[string]any{"alow": []any{}}, "did you mean allow?"},
		"allow not array":  {map[string]any{"allow": "read"}, "tools.allow must be an array of tool names"},
		"allow null":       {map[string]any{"allow": nil}, "tools.allow must be an array of tool names"},
		"entry not string": {map[string]any{"allow": []any{3}}, "tools.allow[0] must be a non-empty string"},
		"entry blank":      {map[string]any{"allow": []any{"  "}}, "tools.allow[0] must be a non-empty string"},
		"unknown native":   {map[string]any{"allow": []any{"raed"}}, `tools.allow[0] "raed" is not a native tool; use one of bash, decide`},
		"mcp without tool": {map[string]any{"allow": []any{"mcp:docs"}}, `"mcp:docs" must be mcp:<server>:<tool> or mcp:<server>:*`},
		"mcp empty tool":   {map[string]any{"allow": []any{"mcp:docs:"}}, `"mcp:docs:" must be mcp:<server>:<tool>`},
		"mcp bad server":   {map[string]any{"allow": []any{"mcp:do.cs:x"}}, `must be mcp:<server>:<tool>`},
		"padded native":    {map[string]any{"allow": []any{" read "}}, `" read " must not contain whitespace`},
		"space in mcp":     {map[string]any{"allow": []any{"mcp:docs:a b"}}, `"mcp:docs:a b" must not contain whitespace`},
		"colon wildcard":   {map[string]any{"allow": []any{"mcp:docs:foo:*"}}, `"mcp:docs:foo:*" must be mcp:<server>:<tool>`},
		"duplicate":        {map[string]any{"allow": []any{"read", "read"}}, `tools.allow[1] repeats "read"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadProfileTools(t, "lean", map[string]any{"tools": tc.tools})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The layout loader reads only the selected profile's file, so a bad tools block in
// another profile is reported when that profile is selected, not before. The legacy
// loader checked every profile; missing.md (Phase 1) records the gap.
func TestProfileToolsOfAnUnselectedProfileAreReportedWhenItIsSelected(t *testing.T) {
	bad := map[string]any{"tools": map[string]any{"allow": []any{"raed"}}}
	if _, err := loadProfileTools(t, "full", bad); err != nil {
		t.Fatalf("unselected profile with a typo: %v (the layout loader does not read it)", err)
	}
	if _, err := loadProfileTools(t, "lean", bad); err == nil || !strings.Contains(err.Error(), `"raed" is not a native tool`) {
		t.Fatalf("selected profile with a typo: error = %v, want it reported", err)
	}
}

// The schema file documents the same keys and entry forms the loader accepts.
func TestProfileToolsSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Paths struct {
				Properties struct {
					Profiles struct {
						AdditionalProperties struct {
							Properties struct {
								Tools struct {
									Properties map[string]json.RawMessage `json:"properties"`
									AnyOf      []struct {
										Required []string `json:"required"`
									} `json:"anyOf"`
									AdditionalProperties bool `json:"additionalProperties"`
								} `json:"tools"`
							} `json:"properties"`
						} `json:"additionalProperties"`
					} `json:"profiles"`
				} `json:"properties"`
			} `json:"paths"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	tools := schema.Properties.Paths.Properties.Profiles.AdditionalProperties.Properties.Tools
	var keys []string
	for key := range tools.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := append([]string(nil), profileToolsKeys...)
	sort.Strings(want)
	var anyOf [][]string
	for _, alternative := range tools.AnyOf {
		anyOf = append(anyOf, alternative.Required)
	}
	if !reflect.DeepEqual(keys, want) || tools.AdditionalProperties || !reflect.DeepEqual(anyOf, [][]string{{"allow"}, {"deny"}}) {
		t.Fatalf("schema keys %v anyOf-required %v additionalProperties %v, loader accepts %v and needs allow or deny", keys, anyOf, tools.AdditionalProperties, want)
	}
	var pattern *regexp.Regexp
	for _, list := range []string{"allow", "deny"} {
		var entries struct {
			Items struct {
				Pattern string `json:"pattern"`
			} `json:"items"`
			UniqueItems bool `json:"uniqueItems"`
		}
		if err := json.Unmarshal(tools.Properties[list], &entries); err != nil {
			t.Fatal(err)
		}
		compiled, err := regexp.Compile(entries.Items.Pattern)
		if err != nil {
			t.Fatalf("%s schema pattern: %v", list, err)
		}
		if pattern != nil && pattern.String() != compiled.String() {
			t.Errorf("allow and deny use different entry patterns: %q and %q", pattern, compiled)
		}
		if !entries.UniqueItems {
			t.Errorf("schema %s must set uniqueItems, the loader refuses a repeated entry", list)
		}
		pattern = compiled
	}
	// Every native tool name the loader knows is allowed by the schema pattern, and
	// the pattern and the loader agree on a sample of good and bad entries.
	for _, name := range nativeToolNames {
		if !pattern.MatchString(name) {
			t.Errorf("schema pattern refuses native tool %q", name)
		}
	}
	for _, entry := range []string{"mcp:docs:search", "mcp:docs:*", "mcp:docs", "mcp:docs:", "mcp:do cs:x", "raed", "Read", ""} {
		loaderOK := checkProfileToolEntry(entry) == nil
		if got := pattern.MatchString(entry); got != loaderOK {
			t.Errorf("entry %q: schema pattern says %v, loader says %v", entry, got, loaderOK)
		}
	}
}
