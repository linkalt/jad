package sessionfs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNativeReadImageSettings(t *testing.T) {
	readPolicy := NativePathPolicy{Paths: []NativePathRule{{Action: "allow", Path: "/tmp/pics"}}}
	absent, err := normalizeNativeReadImageSettings(decideSettingsFor(`{"read":{}}`), "setting.json", readPolicy)
	if err != nil || absent != nil {
		t.Fatalf("a missing read_image key = %+v, err=%v; want the tool off", absent, err)
	}
	noTools, err := normalizeNativeReadImageSettings(map[string]json.RawMessage{}, "setting.json", readPolicy)
	if err != nil || noTools != nil {
		t.Fatalf("no tools key = %+v, err=%v", noTools, err)
	}

	defaults, err := normalizeNativeReadImageSettings(decideSettingsFor(`{"read_image":{}}`), "setting.json", readPolicy)
	if err != nil {
		t.Fatal(err)
	}
	want := &NativeReadImageSettings{Policy: readPolicy, MaxImageBytes: 5 * 1024 * 1024}
	if !reflect.DeepEqual(defaults, want) {
		t.Fatalf("defaults = %+v, want %+v", defaults, want)
	}

	set, err := normalizeNativeReadImageSettings(decideSettingsFor(`{"read_image":{"max_image_bytes":1234}}`), "setting.json", readPolicy)
	if err != nil || set.MaxImageBytes != 1234 {
		t.Fatalf("max_image_bytes = %+v, err=%v", set, err)
	}
	// The policy is a copy of the read policy: changing one must not change the other.
	set.Policy.Paths[0].Path = "/changed"
	if readPolicy.Paths[0].Path != "/tmp/pics" {
		t.Fatal("read_image shares the read policy's storage instead of copying it")
	}
}

func TestNormalizeNativeReadImageSettingsRejectsInvalidSettings(t *testing.T) {
	cases := map[string]struct{ native, want string }{
		"typo":                 {`{"read_image":{"max_image_byte":10}}`, "tools.native.read_image.max_image_byte is not supported; did you mean max_image_bytes?"},
		"paths belong to read": {`{"read_image":{"paths":["/x"]}}`, "tools.native.read_image.paths is not supported; supported keys are max_image_bytes; the path policy is tools.native.read.paths"},
		"not object":           {`{"read_image":[]}`, "tools.native.read_image must be a JSON object"},
		"null":                 {`{"read_image":null}`, "tools.native.read_image must be a JSON object"},
		"zero":                 {`{"read_image":{"max_image_bytes":0}}`, "max_image_bytes"},
		"too large":            {`{"read_image":{"max_image_bytes":99999999999}}`, "max_image_bytes"},
		"not a number":         {`{"read_image":{"max_image_bytes":"big"}}`, "max_image_bytes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeReadImageSettings(decideSettingsFor(tc.native), "setting.json", NativePathPolicy{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
