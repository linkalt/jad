package sessionfs

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// loadLazyTools loads the profile "lean" with the given tools block and a toolset whose
// native.load_tool is loadTool (nil leaves load_tool out).
func loadLazyTools(t *testing.T, profileTools map[string]any, loadTool any) (SessionConfig, error) {
	t.Helper()
	profile := map[string]any{}
	if profileTools != nil {
		profile["tools"] = profileTools
	}
	var native map[string]any
	if loadTool != nil {
		native = map[string]any{"load_tool": loadTool}
	}
	return loadProfileToolsWithNative(t, "lean", profile, native)
}

var allowReadImage = map[string]any{"allow": []any{"read_image", "exec"}}

// tools.native.load_tool owns the lazy list and its two bounds; {} enables the tool with no list
// and the default bounds, and the lazy key alone enables it.
func TestNativeLoadToolOwnsTheLazyKeys(t *testing.T) {
	config, err := loadLazyTools(t, allowReadImage, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	want := &NativeLoadToolSettings{SummaryBytes: defaultLazySummaryBytes, LoadMax: defaultLazyLoadMax}
	if !reflect.DeepEqual(config.NativeLoadTool, want) {
		t.Fatalf("NativeLoadTool = %+v, want the defaults %+v", config.NativeLoadTool, want)
	}
	if defaultLazySummaryBytes != 256 || defaultLazyLoadMax != 16 {
		t.Errorf("defaults %d and %d, want 256 and 16", defaultLazySummaryBytes, defaultLazyLoadMax)
	}
	if config.ProfileTools.Lazy != nil {
		t.Errorf("view Lazy = %v with no list, want nil", config.ProfileTools.Lazy)
	}

	config, err = loadLazyTools(t, map[string]any{"allow": []any{"read_image", "exec", "mcp:docs:search"}},
		map[string]any{"lazy": []any{"web_search", "mcp:docs:search"}, "lazy_summary_bytes": 64, "lazy_load_max": 3})
	if err != nil {
		t.Fatal(err)
	}
	want = &NativeLoadToolSettings{Lazy: []string{"web_search", "mcp:docs:search"}, SummaryBytes: 64, LoadMax: 3}
	if !reflect.DeepEqual(config.NativeLoadTool, want) {
		t.Fatalf("NativeLoadTool = %+v, want %+v", config.NativeLoadTool, want)
	}
	// The view the visibility rule reads is the list, and nothing of the bounds.
	view := ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"read_image", "exec", "mcp:docs:search"}, Lazy: []string{"web_search", "mcp:docs:search"}}
	if !reflect.DeepEqual(config.ProfileTools, view) {
		t.Fatalf("ProfileTools = %+v, want %+v", config.ProfileTools, view)
	}
	// The view is a copy: writing to it never reaches the load_tool config.
	config.ProfileTools.Lazy[0] = "decide_systemone"
	if config.NativeLoadTool.Lazy[0] != "web_search" {
		t.Error("ProfileTools.Lazy shares its backing array with NativeLoadTool.Lazy")
	}
}

// A profile with no tools block can have lazy tools: the list is not the profile's, so the block is
// not needed to carry it. The view is filled whatever the block says, a deny-only block included.
func TestLazyListFillsTheViewWithoutAToolsBlock(t *testing.T) {
	lazy := map[string]any{"lazy": []any{"mcp:docs:*"}}
	config, err := loadLazyTools(t, nil, lazy)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ProfileToolSettings{Lazy: []string{"mcp:docs:*"}}); !reflect.DeepEqual(config.ProfileTools, want) {
		t.Fatalf("ProfileTools = %+v, want %+v (not restricted, list filled)", config.ProfileTools, want)
	}
	config, err = loadLazyTools(t, map[string]any{"deny": []any{"exec"}}, lazy)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ProfileToolSettings{Restricted: true, Deny: []string{"exec"}, Lazy: []string{"mcp:docs:*"}}); !reflect.DeepEqual(config.ProfileTools, want) {
		t.Fatalf("ProfileTools = %+v, want %+v", config.ProfileTools, want)
	}
	// Without load_tool there is no list, so the view stays empty.
	if config, err = loadLazyTools(t, allowReadImage, nil); err != nil || config.NativeLoadTool != nil || config.ProfileTools.Lazy != nil {
		t.Fatalf("no load_tool: %+v, %v", config.ProfileTools, err)
	}
}

// The coupling check is gone: a profile that has a lazy list always has load_tool, because the list
// is load_tool's. A profile with no list and no load_tool loads as before.
func TestNativeLoadToolIsEnabledOnlyByItsKey(t *testing.T) {
	config, err := loadLazyTools(t, map[string]any{"allow": []any{"read_image", "load_tool"}}, nil)
	if err != nil || config.NativeLoadTool != nil {
		t.Fatalf("NativeLoadTool = %+v (%v) without tools.native.load_tool, want nil", config.NativeLoadTool, err)
	}
	config, err = loadLazyTools(t, map[string]any{"allow": []any{"read_image", "load_tool"}}, map[string]any{"lazy": []any{"web_search"}})
	if err != nil || config.NativeLoadTool == nil {
		t.Fatalf("the lazy key alone did not enable load_tool: %+v, %v", config.NativeLoadTool, err)
	}
}

// The bounds are the ones the profile keys had: 1 to 4096 for lazy_summary_bytes and 1 to 100 for
// lazy_load_max, a wrong type or a fraction refused, null the default; each error names the key.
func TestNativeLoadToolBounds(t *testing.T) {
	for key, tc := range map[string]struct {
		min, max, fallback int64
		get                func(*NativeLoadToolSettings) int64
	}{
		"lazy_summary_bytes": {1, 4096, 256, func(s *NativeLoadToolSettings) int64 { return s.SummaryBytes }},
		"lazy_load_max":      {1, 100, 16, func(s *NativeLoadToolSettings) int64 { return s.LoadMax }},
	} {
		want := "tools.native.load_tool." + key + " must be a safe integer between " + itoa(tc.min) + " and " + itoa(tc.max)
		for _, value := range []any{0, tc.max + 1, itoa(tc.fallback), 1.5, true, []any{}, map[string]any{}} {
			if _, err := loadLazyTools(t, allowReadImage, map[string]any{key: value}); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s %v: err = %v, want %q", key, value, err, want)
			}
		}
		for _, value := range []int64{tc.min, tc.max} {
			config, err := loadLazyTools(t, allowReadImage, map[string]any{key: value})
			if err != nil || tc.get(config.NativeLoadTool) != value {
				t.Errorf("%s %d: %+v, %v, want it accepted", key, value, config.NativeLoadTool, err)
			}
		}
		config, err := loadLazyTools(t, allowReadImage, map[string]any{key: nil})
		if err != nil || tc.get(config.NativeLoadTool) != tc.fallback {
			t.Errorf("%s null: %+v, %v, want the default %d", key, config.NativeLoadTool, err, tc.fallback)
		}
	}
}

func itoa(value int64) string {
	text, _ := json.Marshal(value)
	return string(text)
}

// The lazy list follows the entry rules the profile key had: an array of non-blank entries in the
// allow syntax, no repeats, MCP and extension tools only, never a native tool or load_tool. null is
// refused like any non-array.
func TestNativeLoadToolLazyEntries(t *testing.T) {
	for name, tc := range map[string]struct {
		lazy any
		want string
	}{
		"not an array":     {"web_search", "tools.native.load_tool.lazy must be an array of tool names"},
		"null":             {nil, "tools.native.load_tool.lazy must be an array of tool names"},
		"object":           {map[string]any{}, "tools.native.load_tool.lazy must be an array of tool names"},
		"entry not string": {[]any{3}, "tools.native.load_tool.lazy[0] must be a non-empty string"},
		"entry blank":      {[]any{"web_search", " "}, "tools.native.load_tool.lazy[1] must be a non-empty string"},
		"unknown name":     {[]any{"raed"}, `tools.native.load_tool.lazy[0] "raed" is not a native tool`},
		"bad mcp":          {[]any{"mcp:docs"}, `tools.native.load_tool.lazy[0] "mcp:docs" must be mcp:<server>:<tool> or mcp:<server>:*`},
		"whitespace":       {[]any{" web_search"}, `tools.native.load_tool.lazy[0] " web_search" must not contain whitespace`},
		"duplicate":        {[]any{"web_search", "mcp:docs:*", "web_search"}, `tools.native.load_tool.lazy[2] repeats "web_search"`},
		"load_tool":        {[]any{"mcp:docs:search", "load_tool"}, `tools.native.load_tool.lazy[1] "load_tool" cannot be lazy: it is the tool that loads the others`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadLazyTools(t, allowReadImage, map[string]any{"lazy": tc.lazy})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	for _, name := range nativeToolNames {
		if name == LoadToolName {
			continue
		}
		_, err := loadLazyTools(t, allowReadImage, map[string]any{"lazy": []any{"mcp:docs:search", name}})
		if err == nil || !strings.Contains(err.Error(), `tools.native.load_tool.lazy[1] "`+name+`" is a native tool and cannot be lazy`) ||
			!strings.Contains(err.Error(), "mcp:<server>:<tool>") || !strings.Contains(err.Error(), "decide_systemone, web_search") {
			t.Errorf("native %s: err = %v, want the refusal naming the entry and the accepted kinds", name, err)
		}
	}
	for _, entry := range []string{"web_search", "decide_systemone", "mcp:docs:search", "mcp:docs:*"} {
		if _, err := loadLazyTools(t, allowReadImage, map[string]any{"lazy": []any{entry}}); err != nil {
			t.Errorf("lazy entry %s: %v, want it accepted", entry, err)
		}
	}
	if config, err := loadLazyTools(t, allowReadImage, map[string]any{"lazy": []any{}}); err != nil || config.NativeLoadTool == nil || len(config.ProfileTools.Lazy) != 0 {
		t.Errorf("empty list: %+v, %v, want load_tool enabled with no lazy tool", config.NativeLoadTool, err)
	}
}

// An unknown key under load_tool is refused by name with the supported keys, a near miss with a
// suggestion; the value must be an object.
func TestNativeLoadToolRefusesUnknownKeysAndNonObjects(t *testing.T) {
	_, err := loadLazyTools(t, allowReadImage, map[string]any{"max": 1})
	if err == nil || !strings.Contains(err.Error(), "tools.native.load_tool.max is not supported") || !strings.Contains(err.Error(), "supported keys are lazy, lazy_load_max, lazy_summary_bytes") {
		t.Fatalf("err = %v, want the unknown key refused with the three supported keys", err)
	}
	if _, err = loadLazyTools(t, allowReadImage, map[string]any{"lazy_load_mx": 1}); err == nil || !strings.Contains(err.Error(), "did you mean lazy_load_max?") {
		t.Fatalf("err = %v, want the suggestion", err)
	}
	for _, value := range []any{[]any{}, "x", 3, true} {
		if _, err := loadLazyTools(t, allowReadImage, value); err == nil || !strings.Contains(err.Error(), "tools.native.load_tool must be an object") {
			t.Errorf("load_tool %v: err = %v, want it must be an object", value, err)
		}
	}
}

// The three keys left a profile's tools block: each is refused by name, whatever its value (null
// included), and the message says where it went. Several keys give one error, naming the first in
// sorted order.
func TestProfileToolsRefusesTheMovedLazyKeys(t *testing.T) {
	for _, key := range []string{"lazy", "lazy_summary_bytes", "lazy_load_max"} {
		for _, value := range []any{[]any{"web_search"}, 64, nil} {
			_, err := loadLazyTools(t, map[string]any{"allow": []any{"read_image"}, key: value}, map[string]any{})
			want := "tools." + key + " is not a key of a tools block any more"
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "tools.native.load_tool."+key+" in a toolset file the profile names") {
				t.Errorf("%s=%v: err = %v, want %q and where it moved", key, value, err, want)
			}
		}
	}
	// Refused before the block's other rules: a lazy-only block names the key, not "needs allow or deny".
	if _, err := loadLazyTools(t, map[string]any{"lazy": []any{"web_search"}}, map[string]any{}); err == nil || !strings.Contains(err.Error(), "tools.lazy is not a key of a tools block") {
		t.Errorf("lazy-only block: err = %v", err)
	}
	for range 20 {
		_, err := loadLazyTools(t, map[string]any{"allow": []any{}, "lazy_summary_bytes": 1, "lazy_load_max": 2, "lazy": []any{}}, nil)
		if err == nil || !strings.Contains(err.Error(), "tools.lazy is not a key") {
			t.Fatalf("several keys: err = %v, want the first in sorted order, lazy", err)
		}
	}
	// The generic list of supported keys no longer carries them.
	_, err := loadLazyTools(t, map[string]any{"allow": []any{}, "block": []any{}}, nil)
	if err == nil || !strings.Contains(err.Error(), "supported keys are allow, deny") {
		t.Errorf("unknown key: err = %v, want the supported keys allow, deny", err)
	}
}

// An agent's tools object is written like a profile's tools block, so it refuses the moved keys by
// name too (it used to parse and drop them).
func TestAgentToolsObjectRefusesTheMovedLazyKeys(t *testing.T) {
	for _, key := range []string{"lazy", "lazy_summary_bytes", "lazy_load_max"} {
		_, err := loadToolsetSession(t, toolsetSpec(map[string]any{"tools": map[string]any{"deny": []any{"exec"}, key: []any{}}}), nil)
		if err == nil || !strings.Contains(err.Error(), "tools."+key+" is not a key of a tools block any more") || !strings.Contains(err.Error(), "finder") {
			t.Errorf("%s in an agent's tools object: err = %v, want it refused by name", key, err)
		}
	}
	if _, err := loadToolsetSession(t, toolsetSpec(map[string]any{"tools": map[string]any{"deny": []any{"exec"}}}), nil); err != nil {
		t.Errorf("an agent tools object with deny only: %v", err)
	}
}

// The refusal of an agent's tools object says where the key went, as a profile's does: the migration
// message is the whole help the author gets.
func TestAgentToolsObjectRefusalNamesTheNewLocation(t *testing.T) {
	for _, key := range []string{"lazy", "lazy_summary_bytes", "lazy_load_max"} {
		_, err := loadToolsetSession(t, toolsetSpec(map[string]any{"tools": map[string]any{"deny": []any{"exec"}, key: []any{}}}), nil)
		if err == nil || !strings.Contains(err.Error(), "set tools.native.load_tool."+key+" in a toolset file the profile names") {
			t.Errorf("%s in an agent's tools object: err = %v, want the new location named", key, err)
		}
	}
}

// The schemas say what the loader does: the profile's tools block has allow and deny only, and the
// setting schema's load_tool has the three keys with the loader's bounds and its entry pattern.
func TestLoadToolSchemaMatchesTheLoader(t *testing.T) {
	var profile struct {
		Properties struct {
			Tools struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	readJSONFile(t, filepath.Join("..", "..", "..", "sample", "schemes", "profile.schema.json"), &profile)
	for _, key := range []string{"lazy", "lazy_summary_bytes", "lazy_load_max"} {
		if _, found := profile.Properties.Tools.Properties[key]; found {
			t.Errorf("profile.schema.json tools has %s, which the loader refuses by name", key)
		}
	}
	var setting struct {
		Properties struct {
			Tools struct {
				Properties struct {
					Native struct {
						Properties struct {
							LoadTool struct {
								Properties           map[string]json.RawMessage `json:"properties"`
								AdditionalProperties bool                       `json:"additionalProperties"`
							} `json:"load_tool"`
						} `json:"properties"`
					} `json:"native"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	readJSONFile(t, filepath.Join("..", "..", "..", "sample", "schemes", "setting.schema.json"), &setting)
	node := setting.Properties.Tools.Properties.Native.Properties.LoadTool
	if got := slices.Sorted(maps.Keys(node.Properties)); !slices.Equal(got, slices.Sorted(slices.Values(nativeLoadToolKeys))) || node.AdditionalProperties {
		t.Fatalf("setting.schema.json load_tool keys %v (additionalProperties %v), loader accepts %v and refuses others", got, node.AdditionalProperties, nativeLoadToolKeys)
	}
	for key, want := range map[string][3]int64{
		"lazy_summary_bytes": {minLazySummaryBytes, maxLazySummaryBytes, defaultLazySummaryBytes},
		"lazy_load_max":      {minLazyLoadMax, maxLazyLoadMax, defaultLazyLoadMax},
	} {
		var bounds struct {
			Type    string `json:"type"`
			Minimum int64  `json:"minimum"`
			Maximum int64  `json:"maximum"`
			Default int64  `json:"default"`
		}
		if err := json.Unmarshal(node.Properties[key], &bounds); err != nil {
			t.Fatal(err)
		}
		if bounds.Type != "integer" || [3]int64{bounds.Minimum, bounds.Maximum, bounds.Default} != want {
			t.Errorf("%s: schema %+v, loader min/max/default %v", key, bounds, want)
		}
	}
	var lazy struct {
		Type  string `json:"type"`
		Items struct {
			Pattern string `json:"pattern"`
		} `json:"items"`
		UniqueItems bool `json:"uniqueItems"`
	}
	if err := json.Unmarshal(node.Properties["lazy"], &lazy); err != nil {
		t.Fatal(err)
	}
	if lazy.Type != "array" || !lazy.UniqueItems {
		t.Errorf("lazy schema %+v, want a unique array", lazy)
	}
	pattern := regexp.MustCompile(lazy.Items.Pattern)
	for _, name := range nativeToolNames {
		if pattern.MatchString(name) {
			t.Errorf("native tool %q: the schema's lazy pattern admits it, the loader refuses it", name)
		}
	}
	for _, entry := range []string{"decide_systemone", "web_search", "mcp:docs:search", "mcp:docs:*", "mcp:docs", "mcp:docs:", "mcp:do cs:x", "raed", ""} {
		loaderOK := checkProfileToolEntry(entry) == nil && checkLazyEntry(entry) == nil
		if got := pattern.MatchString(entry); got != loaderOK {
			t.Errorf("lazy entry %q: schema pattern says %v, loader says %v", entry, got, loaderOK)
		}
	}
}

// Two toolsets that both name load_tool: the first one in the profile's list wins as a whole object
// (no key-by-key merge across toolsets), and the second is reported with a warning.
func TestFirstToolsetsLoadToolWinsAsAWholeObject(t *testing.T) {
	spec := layouttest.Spec{
		Native:   map[string]any{"load_tool": map[string]any{"lazy": []any{"web_search"}}},
		Profiles: map[string]map[string]any{"dev/lean": {"toolset": []any{"coding/default", "extra/second"}}, "dev/other": {"toolset": []any{"extra/second", "coding/default"}}},
	}
	extra := map[string]string{"extra/second": `{"native":{"load_tool":{"lazy":["mcp:docs:*"],"lazy_load_max":5}}}`}
	fixture := layouttest.Write(t, spec)
	for id, text := range extra {
		writeToolsetFile(t, fixture, id, text)
	}
	cfg, err := Load(LoadOptions{WorkingDir: fixture.Workspace, GlobalDir: fixture.GlobalDir, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := Resolve(cfg.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := LoadSessionSettings(location, "dev/lean", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := (&NativeLoadToolSettings{Lazy: []string{"web_search"}, SummaryBytes: defaultLazySummaryBytes, LoadMax: defaultLazyLoadMax}); !reflect.DeepEqual(first.NativeLoadTool, want) {
		t.Errorf("first toolset coding/default: %+v, want %+v (the second's lazy_load_max 5 is not merged in)", first.NativeLoadTool, want)
	}
	if !slices.ContainsFunc(first.Warnings, func(w string) bool {
		return strings.Contains(w, "native tool load_tool is in toolsets coding/default and extra/second; using the one from coding/default")
	}) {
		t.Errorf("warnings %q, want the second load_tool reported", first.Warnings)
	}
	second, err := LoadSessionSettings(location, "dev/other", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := (&NativeLoadToolSettings{Lazy: []string{"mcp:docs:*"}, SummaryBytes: defaultLazySummaryBytes, LoadMax: 5}); !reflect.DeepEqual(second.NativeLoadTool, want) {
		t.Errorf("first toolset extra/second: %+v, want %+v", second.NativeLoadTool, want)
	}
}

// A profile's own toolset may tune the two bounds of load_tool and never the list: lazy is refused
// by name with the reach reason, and omit.native load_tool leaves the tool out of that profile.
func TestOwnToolsetTunesLoadToolBoundsButNotItsList(t *testing.T) {
	const base = `{"schema_version":1,"native":{"load_tool":{"lazy":["web_search"],"lazy_summary_bytes":100,"lazy_load_max":9}}}`
	load := func(own string) (SessionConfig, error) {
		o := newOwnToolsetFixture(t, leanCoder)
		wsPut(t, o.base, base)
		wsPut(t, o.own, own)
		return o.load(t, "dev/coder")
	}
	config, err := load(`{"based_on":"global:coding/full","set":{"native":{"load_tool":{"lazy_summary_bytes":50,"lazy_load_max":3}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := (&NativeLoadToolSettings{Lazy: []string{"web_search"}, SummaryBytes: 50, LoadMax: 3}); !reflect.DeepEqual(config.NativeLoadTool, want) {
		t.Fatalf("composed load_tool = %+v, want the base's list with the tuned bounds %+v", config.NativeLoadTool, want)
	}
	if !reflect.DeepEqual(config.ProfileTools.Lazy, []string{"web_search"}) {
		t.Errorf("view = %v", config.ProfileTools.Lazy)
	}
	_, err = load(`{"based_on":"global:coding/full","set":{"native":{"load_tool":{"lazy":["mcp:docs:*"]}}}}`)
	if err == nil || !strings.Contains(err.Error(), "set.native.load_tool.lazy cannot be changed by a profile's own toolset: it changes what the tool can reach") || !IsOwnToolsetError(err) {
		t.Fatalf("lazy in an own toolset: %v, want it refused by name with the reach reason", err)
	}
	if _, err = load(`{"based_on":"global:coding/full","set":{"native":{"load_tool":{"nope":1}}}}`); err == nil || !strings.Contains(err.Error(), "set.native.load_tool.nope is not one a profile's own toolset may change (allowed: lazy_load_max, lazy_summary_bytes)") {
		t.Fatalf("unknown key in an own toolset: %v", err)
	}
	if config, err = load(`{"based_on":"global:coding/full","omit":{"native":["load_tool"]}}`); err != nil || config.NativeLoadTool != nil || config.ProfileTools.Lazy != nil {
		t.Fatalf("omit load_tool: %+v %v, want the tool and its list gone", config.NativeLoadTool, err)
	}
}

// A toolset used by an agent follows the owner: a lazy list in its load_tool is refused (the
// children use the owner's list, so it would do nothing), an agent toolset with no load_tool loads
// while the owner has a list, and the derived config carries the owner's tools view wholesale.
func TestAgentToolsetFollowsTheOwnersLazyList(t *testing.T) {
	spec := func() layouttest.Spec {
		spec := toolsetSpec(map[string]any{"toolset": "extra/search"})
		spec.Native["load_tool"] = map[string]any{"lazy": []any{"web_search", "mcp:docs:*"}, "lazy_summary_bytes": 77, "lazy_load_max": 7}
		spec.ProfileExtra = map[string]any{"tools": map[string]any{"allow": []any{"read_image"}, "deny": []any{"exec"}}}
		return spec
	}
	_, err := loadToolsetSession(t, spec(), map[string]string{"extra/search": `{"native":{"read_image":{},"load_tool":{"lazy":["web_search"]}}}`})
	if err == nil || !strings.Contains(err.Error(), "agent finder uses toolset extra/search") || !strings.Contains(err.Error(), "tools.native.load_tool.lazy lists 1 tool(s)") || !strings.Contains(err.Error(), "uses the lazy list of the profile that owns it") {
		t.Fatalf("lazy in an agent toolset: %v, want the refusal naming the agent, the toolset and the rule", err)
	}
	// An empty list and the tuning keys are not a list.
	if _, err := loadToolsetSession(t, spec(), map[string]string{"extra/search": `{"native":{"read_image":{},"load_tool":{"lazy":[]}}}`}); err != nil {
		t.Errorf("an empty lazy list in an agent toolset: %v", err)
	}
	// The owner has a list and the agent toolset has no load_tool: it loads (the old coupling check
	// refused it), and its config carries the owner's view.
	config, err := loadToolsetSession(t, spec(), map[string]string{"extra/search": `{"native":{"read_image":{}}}`})
	if err != nil {
		t.Fatalf("an agent toolset with no load_tool while the owner has a lazy list: %v", err)
	}
	derived, ok := config.AgentToolsets["extra/search"]
	if !ok {
		t.Fatalf("AgentToolsets = %v", slices.Collect(maps.Keys(config.AgentToolsets)))
	}
	if !reflect.DeepEqual(derived.ProfileTools, config.ProfileTools) {
		t.Errorf("derived view = %+v, want the owner's %+v", derived.ProfileTools, config.ProfileTools)
	}
	if want := []string{"web_search", "mcp:docs:*"}; !reflect.DeepEqual(derived.ProfileTools.Lazy, want) || derived.NativeLoadTool != nil {
		t.Errorf("derived Lazy %v load_tool %+v, want the owner's list %v and no load_tool of its own", derived.ProfileTools.Lazy, derived.NativeLoadTool, want)
	}
	// An agent toolset that has load_tool with no list keeps the owner's view too.
	config, err = loadToolsetSession(t, spec(), map[string]string{"extra/search": `{"native":{"read_image":{},"load_tool":{"lazy_load_max":2}}}`})
	if err != nil {
		t.Fatal(err)
	}
	derived = config.AgentToolsets["extra/search"]
	if !reflect.DeepEqual(derived.ProfileTools, config.ProfileTools) || derived.NativeLoadTool == nil || derived.NativeLoadTool.LoadMax != 2 || len(derived.NativeLoadTool.Lazy) != 0 {
		t.Errorf("derived view %+v load_tool %+v, want the owner's view and its own empty load_tool", derived.ProfileTools, derived.NativeLoadTool)
	}
}

// The Go-only field validator that the session loader runs over every paths.profiles entry
// (profile_fields.go) refuses the moved keys by name too.
func TestProfileFieldValidatorRefusesTheMovedLazyKeys(t *testing.T) {
	for _, key := range []string{"lazy", "lazy_summary_bytes", "lazy_load_max"} {
		raw := json.RawMessage(`{"tools":{"allow":[],"` + key + `":1}}`)
		err := validateProfileFields("a", raw, "setting.json: paths.profiles.a")
		if err == nil || !strings.Contains(err.Error(), "paths.profiles.a.tools."+key+" is not a key of a tools block any more") {
			t.Errorf("%s: err = %v, want it refused by name", key, err)
		}
	}
	if err := validateProfileFields("a", json.RawMessage(`{"tools":{"allow":[],"deny":["exec"]}}`), "setting.json: paths.profiles.a"); err != nil {
		t.Errorf("allow and deny: %v", err)
	}
}
