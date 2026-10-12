package sessionfs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNativeReadImageSettings(t *testing.T) {
	const root = "/jad"
	absent, err := normalizeNativeReadImageSettings(decideSettingsFor(`{"read":{"paths":{"allow":["/tmp/pics"]}}}`), root, "setting.json")
	if err != nil || absent != nil {
		t.Fatalf("a missing read_image key = %+v, err=%v; want the tool off", absent, err)
	}
	noTools, err := normalizeNativeReadImageSettings(map[string]json.RawMessage{}, root, "setting.json")
	if err != nil || noTools != nil {
		t.Fatalf("no tools key = %+v, err=%v", noTools, err)
	}

	// read's rules are not read_image's: without its own paths it gets the empty default.
	defaults, err := normalizeNativeReadImageSettings(decideSettingsFor(`{"read":{"paths":{"allow":["/tmp/pics"]}},"read_image":{}}`), root, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	want := &NativeReadImageSettings{Policy: NativePathPolicy{Paths: []NativePathRule{}}, MaxImageBytes: 5 * 1024 * 1024}
	if !reflect.DeepEqual(defaults, want) {
		t.Fatalf("defaults = %+v, want %+v", defaults, want)
	}

	set, err := normalizeNativeReadImageSettings(decideSettingsFor(`{"read_image":{"max_image_bytes":1234,"paths":{"allow":["/tmp/pics"],"deny":["private"]}}}`), root, "setting.json")
	if err != nil || set.MaxImageBytes != 1234 {
		t.Fatalf("max_image_bytes = %+v, err=%v", set, err)
	}
	wantPolicy := NativePathPolicy{Paths: []NativePathRule{{Action: "allow", Path: "/tmp/pics"}, {Action: "deny", Path: "/jad/private"}}}
	if !reflect.DeepEqual(set.Policy, wantPolicy) {
		t.Fatalf("own paths = %+v, want %+v", set.Policy, wantPolicy)
	}
}

func TestNormalizeNativeReadImageSettingsRejectsInvalidSettings(t *testing.T) {
	cases := map[string]struct{ native, want string }{
		"typo":         {`{"read_image":{"max_image_byte":10}}`, "tools.native.read_image.max_image_byte is not supported; did you mean max_image_bytes?"},
		"bad paths":    {`{"read_image":{"paths":null}}`, "tools.native.read_image.paths"},
		"not object":   {`{"read_image":[]}`, "tools.native.read_image must be a JSON object"},
		"null":         {`{"read_image":null}`, "tools.native.read_image must be a JSON object"},
		"zero":         {`{"read_image":{"max_image_bytes":0}}`, "max_image_bytes"},
		"too large":    {`{"read_image":{"max_image_bytes":99999999999}}`, "max_image_bytes"},
		"not a number": {`{"read_image":{"max_image_bytes":"big"}}`, "max_image_bytes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeReadImageSettings(decideSettingsFor(tc.native), "/jad", "setting.json")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
