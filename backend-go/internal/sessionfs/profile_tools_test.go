package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func profileSetting(profile map[string]any) map[string]any {
	return map[string]any{"paths": map[string]any{
		"active_profile": "lean",
		"profiles":       map[string]any{"lean": profile, "full": map[string]any{}},
	}}
}

func TestProfileToolsLoadAndReachTheConfig(t *testing.T) {
	allow := []any{"read", "mcp:docs:search", "mcp:docs:*"}
	config, err := loadApprovalSetting(t, profileSetting(map[string]any{"tools": map[string]any{"allow": allow}}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := ProfileToolSettings{Restricted: true, Allow: []string{"read", "mcp:docs:search", "mcp:docs:*"}}
	if !reflect.DeepEqual(config.ProfileTools, want) {
		t.Fatalf("ProfileTools = %+v, want %+v", config.ProfileTools, want)
	}
}

func TestProfileToolsEmptyAllowIsRestrictedAndHidesEverything(t *testing.T) {
	config, err := loadApprovalSetting(t, profileSetting(map[string]any{"tools": map[string]any{"allow": []any{}}}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !config.ProfileTools.Restricted || len(config.ProfileTools.Allow) != 0 {
		t.Fatalf("ProfileTools = %+v, want restricted with no entries", config.ProfileTools)
	}
}

// A profile without a tools block, and a session without profiles, are not restricted.
func TestProfileToolsAbsentMeansUnrestricted(t *testing.T) {
	for name, setting := range map[string]map[string]any{
		"profile without tools": profileSetting(map[string]any{}),
		"no profiles":           {},
	} {
		config, err := loadApprovalSetting(t, setting)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if config.ProfileTools.Restricted || config.ProfileTools.Allow != nil {
			t.Errorf("%s: ProfileTools = %+v, want the zero value", name, config.ProfileTools)
		}
	}
}

// Only the selected profile's block applies: another profile's restriction is not read.
func TestProfileToolsApplyToTheSelectedProfileOnly(t *testing.T) {
	setting := map[string]any{"paths": map[string]any{
		"active_profile": "full",
		"profiles": map[string]any{
			"lean": map[string]any{"tools": map[string]any{"allow": []any{"read"}}},
			"full": map[string]any{},
		},
	}}
	config, err := loadApprovalSetting(t, setting)
	if err != nil {
		t.Fatal(err)
	}
	if config.Profile != "full" || config.ProfileTools.Restricted {
		t.Fatalf("profile %q restricted %v, want full and unrestricted", config.Profile, config.ProfileTools.Restricted)
	}
}

func TestProfileToolsShapeErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		tools any
		want  string
	}{
		"not an object":    {[]any{"read"}, "paths.profiles.lean.tools must be an object"},
		"null":             {nil, "paths.profiles.lean.tools must be an object"},
		"no allow":         {map[string]any{}, "tools.allow is required; use an empty array to hide every tool"},
		"unknown key":      {map[string]any{"allow": []any{}, "deny": []any{}}, "tools.deny is not supported"},
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
			_, err := loadApprovalSetting(t, profileSetting(map[string]any{"tools": tc.tools}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The block is checked for every profile, not only the selected one, like the other
// profile fields, so a typo cannot hide until someone switches to that profile.
func TestProfileToolsOfAnUnselectedProfileAreValidatedToo(t *testing.T) {
	setting := map[string]any{"paths": map[string]any{
		"active_profile": "full",
		"profiles": map[string]any{
			"lean": map[string]any{"tools": map[string]any{"allow": []any{"raed"}}},
			"full": map[string]any{},
		},
	}}
	if _, err := loadApprovalSetting(t, setting); err == nil || !strings.Contains(err.Error(), `"raed" is not a native tool`) {
		t.Fatalf("error = %v, want the unselected profile's typo reported", err)
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
									Properties           map[string]json.RawMessage `json:"properties"`
									Required             []string                   `json:"required"`
									AdditionalProperties bool                       `json:"additionalProperties"`
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
	if !reflect.DeepEqual(keys, want) || tools.AdditionalProperties || !reflect.DeepEqual(tools.Required, []string{"allow"}) {
		t.Fatalf("schema keys %v required %v additionalProperties %v, loader accepts %v and requires allow", keys, tools.Required, tools.AdditionalProperties, want)
	}
	var allow struct {
		Items struct {
			Pattern string `json:"pattern"`
		} `json:"items"`
	}
	if err := json.Unmarshal(tools.Properties["allow"], &allow); err != nil {
		t.Fatal(err)
	}
	pattern, err := regexp.Compile(allow.Items.Pattern)
	if err != nil {
		t.Fatalf("schema pattern: %v", err)
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
