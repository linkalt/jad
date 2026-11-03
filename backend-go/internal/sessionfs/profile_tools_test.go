package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// loadProfileTools loads a layout session with profiles dev/lean (the given file) and
// dev/full (an empty one); active names the selected one.
func loadProfileTools(t *testing.T, active string, lean map[string]any) (SessionConfig, error) {
	t.Helper()
	return loadProfileToolsWithNative(t, active, lean, nil)
}

// loadProfileToolsWithNative is loadProfileTools with the toolset's native block, for the tests
// that enable tools.native.load_tool beside tools.lazy. Explicit profiles get no toolset from
// layouttest, so the toolset is named here, on each profile.
func loadProfileToolsWithNative(t *testing.T, active string, lean map[string]any, native map[string]any) (SessionConfig, error) {
	t.Helper()
	profiles := map[string]map[string]any{"dev/lean": lean, "dev/full": {}}
	if native != nil {
		// layouttest writes the toolset only for its own profile, so the explicit ones name it.
		profiles = map[string]map[string]any{"dev/lean": withToolset(lean), "dev/full": withToolset(map[string]any{})}
	}
	location, _ := layoutLocation(t, layouttest.Spec{Native: native, Profiles: profiles})
	return LoadSessionSettings(location, "dev/"+active, "")
}

// withToolset returns a copy of the profile that names the "coding/default" toolset.
func withToolset(profile map[string]any) map[string]any {
	out := make(map[string]any, len(profile)+1)
	for key, value := range profile {
		out[key] = value
	}
	out["toolset"] = "coding/default"
	return out
}

func TestProfileToolsLoadAndReachTheConfig(t *testing.T) {
	allow := []any{"read", "mcp:docs:search", "mcp:docs:*"}
	config, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": allow}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"read", "mcp:docs:search", "mcp:docs:*"}, Deny: nil, LazySummaryBytes: defaultLazySummaryBytes, LazyLoadMax: defaultLazyLoadMax}
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
	want := ProfileToolSettings{Restricted: true, HasAllow: false, Deny: []string{"bash", "mcp:docs:*"}, LazySummaryBytes: defaultLazySummaryBytes, LazyLoadMax: defaultLazyLoadMax}
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
	want := ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"read", "bash"}, Deny: []string{"bash"}, LazySummaryBytes: defaultLazySummaryBytes, LazyLoadMax: defaultLazyLoadMax}
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
		"unknown native":   {map[string]any{"allow": []any{"raed"}}, `tools.allow[0] "raed" is not a native tool; use one of async, bash, decide_systemone`},
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

// A bad tools block in a profile of the session's scope is reported when the session
// loads, whichever profile is selected.
func TestProfileToolsOfAnUnselectedProfileAreReportedAtLoad(t *testing.T) {
	bad := map[string]any{"tools": map[string]any{"allow": []any{"raed"}}}
	for _, active := range []string{"full", "lean"} {
		_, err := loadProfileTools(t, active, bad)
		if err == nil || !strings.Contains(err.Error(), `"raed" is not a native tool`) {
			t.Fatalf("active %s: error = %v, want the typo reported", active, err)
		}
		if active == "full" && !strings.Contains(err.Error(), "lean.json") {
			t.Fatalf("active full: error = %v, want it to name lean.json", err)
		}
	}
}

// Every profile in scope is checked the way the selected one is; a scope that leaves a
// broken profile out loads.
func TestUnselectedProfilesAreCheckedAtLoad(t *testing.T) {
	for name, tc := range map[string]struct {
		lean map[string]any
		want string
	}{
		"settings key":     {map[string]any{"settings": map[string]any{"paths": map[string]any{}}}, "settings may not carry paths"},
		"schema version":   {map[string]any{"schema_version": 7}, "unsupported schema_version 7"},
		"unknown field":    {map[string]any{"colour": "red"}, "colour"},
		"unknown model":    {map[string]any{"models": []any{"nope/none"}}, "models:"},
		"model default":    {map[string]any{"model_default": "nope/none"}, "model_default:"},
		"blank instruct":   {map[string]any{"instructions": "  "}, "instructions must be a non-empty string"},
		"null instruct":    {map[string]any{"instructions": nil}, "instructions must be a non-empty string"},
		"blank prompt doc": {map[string]any{"prompt_documents": []any{" "}}, "prompt_documents[0] must be a non-empty string"},
		"tools not object": {map[string]any{"tools": []any{"read"}}, "tools must be an object"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadProfileTools(t, "full", tc.lean)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "lean.json") {
				t.Fatalf("error = %v, want it to name lean.json and contain %q", err, tc.want)
			}
		})
	}
}

// A profile the session's scope leaves out is not read, so a broken file there loads.
func TestProfileOutsideTheScopeIsNotChecked(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{
		Profiles: map[string]map[string]any{"dev/lean": {"instructions": " "}, "dev/full": {}},
	})
	editJSON(t, location.File, func(m map[string]any) {
		m["scope"] = map[string]any{"profiles": []any{"dev/full"}}
	})
	if _, err := LoadSessionSettings(location, "", ""); err != nil {
		t.Fatalf("load with the broken profile out of scope: %v", err)
	}
}

// The schema file documents the same keys and entry forms the loader accepts.
func TestProfileToolsSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/profile.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Tools struct {
				Properties map[string]json.RawMessage `json:"properties"`
				AnyOf      []struct {
					Required []string `json:"required"`
				} `json:"anyOf"`
				AdditionalProperties bool `json:"additionalProperties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	tools := schema.Properties.Tools
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

// lazy lists tools shown by name and summary; lazy_summary_bytes is its bound, with a default.
func TestProfileToolsLazyLoadsWithItsSummaryBound(t *testing.T) {
	config, err := loadProfileToolsWithNative(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read", "bash", "mcp:docs:search"}, "lazy": []any{"bash", "mcp:docs:search"}, "lazy_summary_bytes": 64}}, map[string]any{"load_tool": map[string]any{}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"read", "bash", "mcp:docs:search"}, Lazy: []string{"bash", "mcp:docs:search"}, LazySummaryBytes: 64, LazyLoadMax: defaultLazyLoadMax}
	if !reflect.DeepEqual(config.ProfileTools, want) {
		t.Fatalf("ProfileTools = %+v, want %+v", config.ProfileTools, want)
	}
}

// lazy_summary_bytes outside 1 to 4096 is a configuration error that names the key.
func TestProfileToolsLazySummaryBytesBounds(t *testing.T) {
	for _, value := range []any{0, 4097, "256", 1.5} {
		_, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read"}, "lazy_summary_bytes": value}})
		if err == nil || !strings.Contains(err.Error(), "tools.lazy_summary_bytes must be a safe integer between 1 and 4096") {
			t.Errorf("lazy_summary_bytes %v: err = %v, want the bound error", value, err)
		}
	}
}

// lazy_load_max outside 1 to 100 is a configuration error that names the key; the default is 16.
func TestProfileToolsLazyLoadMaxBounds(t *testing.T) {
	for _, value := range []any{0, 101, "16", 2.5} {
		_, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read"}, "lazy_load_max": value}})
		if err == nil || !strings.Contains(err.Error(), "tools.lazy_load_max must be a safe integer between 1 and 100") {
			t.Errorf("lazy_load_max %v: err = %v, want the bound error", value, err)
		}
	}
	config, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read"}, "lazy_load_max": 3}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.ProfileTools.LazyLoadMax != 3 {
		t.Errorf("LazyLoadMax = %d, want 3", config.ProfileTools.LazyLoadMax)
	}
}

// load_tool is the tool that shows the lazy ones, so it cannot be lazy itself.
func TestProfileToolsRefusesLoadToolAsLazy(t *testing.T) {
	_, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"load_tool", "read"}, "lazy": []any{"read", "load_tool"}}})
	if err == nil || !strings.Contains(err.Error(), `tools.lazy[1] "load_tool" cannot be lazy`) {
		t.Fatalf("err = %v, want the load_tool refusal naming tools.lazy[1]", err)
	}
}

// load_tool is a normal native tool: it is in the session's native settings only when the key
// is present under tools.native, and the profile does not add it.
func TestNativeLoadToolIsEnabledOnlyByItsKey(t *testing.T) {
	config, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read", "load_tool"}}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.NativeLoadTool != nil {
		t.Fatalf("NativeLoadTool = %+v without tools.native.load_tool, want nil", config.NativeLoadTool)
	}
	config, err = loadProfileToolsWithNative(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read", "load_tool"}}}, map[string]any{"load_tool": map[string]any{}})
	if err != nil {
		t.Fatalf("load with tools.native.load_tool: %v", err)
	}
	if config.NativeLoadTool == nil {
		t.Fatal("NativeLoadTool is nil with tools.native.load_tool set")
	}
}

// A profile with tools.lazy is refused at load when tools.native.load_tool is not enabled, and
// the error names both keys. With the key enabled the same profile loads.
func TestLazyWithoutNativeLoadToolIsRefusedAtLoad(t *testing.T) {
	lazy := map[string]any{"tools": map[string]any{"allow": []any{"read", "bash"}, "lazy": []any{"bash"}}}
	_, err := loadProfileTools(t, "lean", lazy)
	if err == nil || !strings.Contains(err.Error(), "tools.lazy") || !strings.Contains(err.Error(), "tools.native.load_tool") {
		t.Fatalf("err = %v, want a refusal naming tools.lazy and tools.native.load_tool", err)
	}
	if _, err := loadProfileToolsWithNative(t, "lean", lazy, map[string]any{"load_tool": map[string]any{}}); err != nil {
		t.Fatalf("lazy with tools.native.load_tool: %v", err)
	}
}

// tools.native.load_tool with no tools.lazy loads: the tool is listed and lists nothing.
func TestNativeLoadToolWithoutLazyLoads(t *testing.T) {
	_, err := loadProfileToolsWithNative(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read", "load_tool"}}}, map[string]any{"load_tool": map[string]any{}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
}

// tools.native.load_tool takes no keys: a key under it is refused by name.
func TestNativeLoadToolRefusesKeys(t *testing.T) {
	_, err := loadProfileToolsWithNative(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read"}}}, map[string]any{"load_tool": map[string]any{"max": 1}})
	if err == nil || !strings.Contains(err.Error(), "tools.native.load_tool takes no keys") {
		t.Fatalf("err = %v, want the no-keys refusal", err)
	}
}

// The supported-keys error for tools.native lists load_tool, so a typo near it is suggested.
func TestNativeLoadToolIsInTheSupportedKeys(t *testing.T) {
	_, err := loadProfileToolsWithNative(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read"}}}, map[string]any{"load_tol": map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "load_tool") {
		t.Fatalf("err = %v, want the supported keys to name load_tool", err)
	}
}

// A lazy entry follows the allow entry syntax: a whitespace or a bad form is refused.
func TestProfileToolsLazyEntriesUseTheAllowSyntax(t *testing.T) {
	for _, entry := range []string{"raed", "mcp:docs", " read"} {
		_, err := loadProfileTools(t, "lean", map[string]any{"tools": map[string]any{"allow": []any{"read"}, "lazy": []any{entry}}})
		if err == nil || !strings.Contains(err.Error(), "tools.lazy[0]") {
			t.Errorf("lazy entry %q: err = %v, want a tools.lazy[0] error", entry, err)
		}
	}
}

// The lazy entries are refused for load_tool, so the schema refuses it too: its pattern is the
// allow and deny pattern without load_tool. allow and deny still take it.
func TestProfileToolsSchemaRefusesLoadToolAsALazyEntry(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/profile.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Tools struct {
				Properties map[string]struct {
					Items struct {
						Pattern string `json:"pattern"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties.Tools.Properties
	lazy := regexp.MustCompile(properties["lazy"].Items.Pattern)
	allow := regexp.MustCompile(properties["allow"].Items.Pattern)
	if lazy.MatchString(LoadToolName) {
		t.Errorf("the schema's lazy pattern admits %q, which the loader refuses as a lazy entry", LoadToolName)
	}
	if !allow.MatchString(LoadToolName) {
		t.Errorf("the schema's allow pattern refuses %q, which the loader accepts", LoadToolName)
	}
	for _, entry := range []string{"read", "bash", "decide_systemone", "mcp:docs:search", "mcp:docs:*"} {
		if !lazy.MatchString(entry) {
			t.Errorf("the schema's lazy pattern refuses %q, which the loader accepts", entry)
		}
	}
	// The late-built tools and subagent are bound by the lazy pass, so the loader accepts them as lazy entries.
	for _, entry := range []string{"subagent", "profile_message", "transcript", "transcripts", "profile"} {
		if !lazy.MatchString(entry) {
			t.Errorf("the schema's lazy pattern refuses %q, which the loader accepts as a lazy entry", entry)
		}
	}
}

// The schema's lazy pattern admits exactly the native names the loader accepts as lazy entries: every native
// tool except load_tool. A name added to nativeToolNames must reach the lazy pattern too, or this fails.
func TestProfileToolsSchemaLazyPatternAgreesWithTheLoaderForEveryNativeName(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/profile.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Tools struct {
				Properties map[string]struct {
					Items struct {
						Pattern string `json:"pattern"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	lazy := regexp.MustCompile(schema.Properties.Tools.Properties["lazy"].Items.Pattern)
	for _, name := range nativeToolNames {
		admitted := lazy.MatchString(name)
		want := name != LoadToolName
		if admitted != want {
			t.Errorf("native tool %q: the schema's lazy pattern admits it = %v, the loader wants %v", name, admitted, want)
		}
	}
}

// A tools block with lazy and neither allow nor deny is refused by the loader, as the schema's
// anyOf refuses it: the block must say which tools it shows.
func TestLazyOnlyToolsBlockIsRefusedLikeTheSchema(t *testing.T) {
	_, err := loadProfileToolsWithNative(t, "lean", map[string]any{"tools": map[string]any{"lazy": []any{"bash"}}}, map[string]any{"load_tool": map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "needs allow or deny") {
		t.Fatalf("err = %v, want the loader to refuse a lazy-only tools block", err)
	}
}
