package anthropicadapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const samplePath = "../../../sample/anthropic-adapter.json"

// The shipped sample must be a valid config, by the schema and by the loader,
// and it must give each model family the profile its comment claims.
func TestSampleConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := compileSchema(t).Validate(instance); err != nil {
		t.Fatalf("the sample does not validate against the schema: %v", err)
	}
	if _, err := LoadConfig(samplePath); err != nil {
		t.Fatalf("the sample does not load: %v", err)
	}
}

// Every sample variant next to the shipped one (a local copy or an untracked
// -api / -oauth file) must also validate and load, so an edit that breaks one
// shows up here and not at startup.
func TestSampleConfigVariantsAreValid(t *testing.T) {
	paths, err := filepath.Glob("../../../sample/anthropic-adapter*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no sample/anthropic-adapter*.json found")
	}
	schema := compileSchema(t)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		instance, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if err := schema.Validate(instance); err != nil {
			t.Errorf("%s does not validate against the schema: %v", path, err)
		}
		if _, err := LoadConfig(path); err != nil {
			t.Errorf("%s does not load: %v", path, err)
		}
	}
}

// The shipped sample pins its own values (the public API URL, a 16384 max), so a
// local edit such as a localhost base_url or a larger max cannot be committed into it.
func TestShippedSampleKeepsItsShippedValues(t *testing.T) {
	config, err := LoadConfig(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	if config.Upstream.BaseURL != DefaultUpstreamBaseURL {
		t.Errorf("upstream.base_url = %q, want %q", config.Upstream.BaseURL, DefaultUpstreamBaseURL)
	}
	if config.DefaultMaxTokens != 16384 {
		t.Errorf("default_max_tokens = %d, want 16384", config.DefaultMaxTokens)
	}
}

func TestSampleConfigProfiles(t *testing.T) {
	config, err := LoadConfig(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		model    string
		thinking string
		midSys   bool
	}{
		{"claude-opus-5-5", ThinkingAdaptive, true},
		{"claude-sonnet-5-5", ThinkingAdaptive, true},
		{"claude-haiku-5-5", ThinkingAdaptive, true},
		{"claude-fable-5-1", ThinkingAdaptive, true},
		{"claude-opus-5", ThinkingAdaptive, true},
		// Sonnet 5 is the one current model without mid-conversation system messages.
		{"claude-sonnet-5", ThinkingAdaptive, false},
		{"claude-opus-4-8", ThinkingAdaptive, true},
		{"claude-opus-4-7", ThinkingAdaptive, false},
		{"claude-sonnet-4-6", ThinkingAdaptive, false},
		{"claude-opus-4-5", ThinkingBudget, false},
		{"claude-sonnet-4-5", ThinkingBudget, false},
		{"claude-haiku-4-5", ThinkingBudget, false},
		{"gpt-5", ThinkingNone, false},
	}
	for _, tc := range cases {
		profile := config.ProfileFor(tc.model)
		if profile.Thinking != tc.thinking || profile.MidConversationSystem != tc.midSys {
			t.Errorf("%s: thinking=%s mid_conversation_system=%v, want %s/%v",
				tc.model, profile.Thinking, profile.MidConversationSystem, tc.thinking, tc.midSys)
		}
	}
}

// A request through the sample config must produce what the docs require for a
// current model: adaptive thinking with an effort, never budget_tokens.
func TestSampleConfigRequests(t *testing.T) {
	config, err := LoadConfig(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	current := translateOK(t, config, `{"model":"claude-opus-5-5","input":"x","reasoning":{"effort":"high"}}`)
	if asJSON(t, current.Thinking) != `{"type":"adaptive","display":"summarized"}` ||
		asJSON(t, current.OutputConfig) != `{"effort":"high"}` {
		t.Fatalf("current model request wrong: %s %s", asJSON(t, current.Thinking), asJSON(t, current.OutputConfig))
	}
	legacy := translateOK(t, config, `{"model":"claude-haiku-4-5","input":"x","max_output_tokens":20000,"reasoning":{"effort":"medium"}}`)
	if asJSON(t, legacy.Thinking) != `{"type":"enabled","budget_tokens":8192,"display":"summarized"}` || legacy.OutputConfig != nil {
		t.Fatalf("budget-mode request wrong: %s %s", asJSON(t, legacy.Thinking), asJSON(t, legacy.OutputConfig))
	}
	cached := translateOK(t, config, cacheBody)
	if got := strings.Join(markedPaths(t, cached), ","); got != "system1,msg2.0" {
		t.Fatalf("the sample's cache settings placed %q", got)
	}
}
