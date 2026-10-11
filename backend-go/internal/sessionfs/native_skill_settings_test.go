package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
)

func skillSettingsFor(native string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":` + native + `}`)}
}

func TestNormalizeNativeSkillSettingsDefaultsAndOverrides(t *testing.T) {
	absent, err := normalizeNativeSkillSettings(skillSettingsFor(`{}`), "setting.json")
	if err != nil || absent != nil {
		t.Fatalf("absent skill = %+v, %v; want nil", absent, err)
	}
	noTools, err := normalizeNativeSkillSettings(map[string]json.RawMessage{}, "setting.json")
	if err != nil || noTools != nil {
		t.Fatalf("no tools = %+v, %v; want nil", noTools, err)
	}
	defaults, err := normalizeNativeSkillSettings(skillSettingsFor(`{"skill":{}}`), "setting.json")
	if err != nil || defaults == nil || defaults.MaxBytes != 131072 {
		t.Fatalf("default skill = %+v, %v; want max_bytes 131072", defaults, err)
	}
	nullBytes, err := normalizeNativeSkillSettings(skillSettingsFor(`{"skill":{"max_bytes":null}}`), "setting.json")
	if err != nil || nullBytes.MaxBytes != 131072 {
		t.Fatalf("null max_bytes = %+v, %v; want the default", nullBytes, err)
	}
	custom, err := normalizeNativeSkillSettings(skillSettingsFor(`{"skill":{"max_bytes":2048}}`), "setting.json")
	if err != nil || custom.MaxBytes != 2048 {
		t.Fatalf("custom skill = %+v, %v", custom, err)
	}
}

func TestNormalizeNativeSkillSettingsRejectsInvalidSettings(t *testing.T) {
	for name, tc := range map[string]struct{ native, want string }{
		"typo":        {`{"skill":{"max_byte":10}}`, "setting.json: tools.native.skill.max_byte is not supported; did you mean max_bytes?"},
		"unrelated":   {`{"skill":{"timeout_ms":10}}`, "setting.json: tools.native.skill.timeout_ms is not supported; supported keys are max_bytes"},
		"not object":  {`{"skill":[]}`, "setting.json: tools.native.skill must be an object"},
		"null":        {`{"skill":null}`, "setting.json: tools.native.skill must be an object"},
		"zero":        {`{"skill":{"max_bytes":0}}`, "setting.json: tools.native.skill.max_bytes must be a safe integer between 1 and 10485760"},
		"fractional":  {`{"skill":{"max_bytes":1.5}}`, "tools.native.skill.max_bytes must be a safe integer"},
		"string":      {`{"skill":{"max_bytes":"10"}}`, "tools.native.skill.max_bytes must be a safe integer"},
		"over limit":  {`{"skill":{"max_bytes":10485761}}`, "tools.native.skill.max_bytes must be a safe integer between 1 and 10485760"},
		"upper limit": {`{"skill":{"max_bytes":10485760}}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeSkillSettings(skillSettingsFor(tc.native), "setting.json")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected a valid setting: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadSessionConfigAcceptsTheSkillToolAndKeepsTheGlobalRoot(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{Native: map[string]any{"skill": map[string]any{"max_bytes": 4096}}})
	rootDir := fixture.Root
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if config.NativeSkill == nil || config.NativeSkill.MaxBytes != 4096 || config.GlobalJadRoot != rootDir {
		t.Fatalf("skill = %+v, global root = %q; want max_bytes 4096 and %q", config.NativeSkill, config.GlobalJadRoot, rootDir)
	}
}
