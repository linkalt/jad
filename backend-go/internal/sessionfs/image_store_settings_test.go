package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestImageStoreDefaultsToOff(t *testing.T) {
	config, err := loadSetting(t, map[string]any{})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	want := ImageStoreSettings{MinBytes: 16 * 1024, MaxFileBytes: 20 << 20, MaxTotalBytes: 1 << 30}
	if config.ImageStore != want || config.ImageStore.Active() {
		t.Fatalf("default image_store = %+v, want %+v and inactive", config.ImageStore, want)
	}
}

func TestImageStoreRelativeDirIsResolvedAgainstTheSessionDirectory(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"image_store": map[string]any{"dir": "pics/../images"}})
	if err != nil {
		t.Fatal(err)
	}
	if !config.ImageStore.Active() || filepath.Base(config.ImageStore.Dir) != "images" || filepath.Base(filepath.Dir(config.ImageStore.Dir)) != "session-1" {
		t.Fatalf("dir = %q, want <session>/images", config.ImageStore.Dir)
	}
}

func TestImageStoreAbsoluteDirAndEveryLimit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	config, err := loadSetting(t, map[string]any{"image_store": map[string]any{"dir": dir, "min_bytes": 100, "max_file_bytes": 5000, "max_total_bytes": 9000}})
	if err != nil {
		t.Fatal(err)
	}
	want := ImageStoreSettings{Dir: dir, MinBytes: 100, MaxFileBytes: 5000, MaxTotalBytes: 9000}
	if config.ImageStore != want {
		t.Fatalf("image_store = %+v, want %+v", config.ImageStore, want)
	}
}

func TestImageStoreRejectsBadValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
		want  string
	}{
		{"not an object", "on", "image_store must be an object"},
		{"null", nil, "image_store must be an object"},
		{"misspelt key", map[string]any{"directory": "x"}, "image_store.directory is not supported; supported keys are dir, max_file_bytes, max_total_bytes, min_bytes"},
		{"dir empty", map[string]any{"dir": " "}, "image_store.dir must be a non-empty path"},
		{"dir number", map[string]any{"dir": 3}, "image_store.dir must be a non-empty path"},
		{"dir null", map[string]any{"dir": nil}, "image_store.dir must be a non-empty path"},
		{"min zero", map[string]any{"min_bytes": 0}, "image_store.min_bytes must be a safe integer between 1 and 67108864"},
		{"min float", map[string]any{"min_bytes": 10.5}, "image_store.min_bytes must be a safe integer"},
		{"min string", map[string]any{"min_bytes": "1"}, "image_store.min_bytes must be a safe integer"},
		{"min null", map[string]any{"min_bytes": nil}, "image_store.min_bytes must be a safe integer"},
		{"file low", map[string]any{"max_file_bytes": 1023}, "image_store.max_file_bytes must be a safe integer between 1024 and 104857600"},
		{"file high", map[string]any{"max_file_bytes": 104857601}, "image_store.max_file_bytes must be a safe integer between 1024 and 104857600"},
		{"total low", map[string]any{"max_total_bytes": 1023}, "image_store.max_total_bytes must be a safe integer between 1024 and 1099511627776"},
		{"total high", map[string]any{"max_total_bytes": int64(1)<<40 + 1}, "image_store.max_total_bytes must be a safe integer between 1024 and 1099511627776"},
		{"min above file", map[string]any{"min_bytes": 5000, "max_file_bytes": 4000}, "min_bytes (5000) must not be above max_file_bytes (4000)"},
		{"file above total", map[string]any{"min_bytes": 100, "max_file_bytes": 5000, "max_total_bytes": 4000}, "max_file_bytes (5000) must not be above max_total_bytes (4000)"},
		{"default file above small total", map[string]any{"max_total_bytes": 4000}, "max_file_bytes (20971520) must not be above max_total_bytes (4000)"},
	} {
		_, err := loadSetting(t, map[string]any{"image_store": test.value})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err = %v, want it to contain %q", test.name, err, test.want)
		}
	}
}

func TestImageStoreSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			ImageStore struct {
				AdditionalProperties bool `json:"additionalProperties"`
				Properties           map[string]struct {
					Type      string          `json:"type"`
					Default   json.RawMessage `json:"default"`
					Minimum   *int64          `json:"minimum"`
					Maximum   *int64          `json:"maximum"`
					MinLength *int            `json:"minLength"`
				} `json:"properties"`
			} `json:"image_store"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties.ImageStore.Properties
	var keys []string
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, imageStoreKeys) || schema.Properties.ImageStore.AdditionalProperties {
		t.Fatalf("schema keys %v (additionalProperties %v), loader accepts %v", keys, schema.Properties.ImageStore.AdditionalProperties, imageStoreKeys)
	}
	if properties["dir"].Type != "string" || properties["dir"].MinLength == nil || *properties["dir"].MinLength != 1 {
		t.Errorf("schema dir = %+v", properties["dir"])
	}
	for name, limits := range map[string][3]int64{
		"min_bytes":       {defaultImageStoreMinBytes, minImageStoreMinBytes, maxImageStoreMinBytes},
		"max_file_bytes":  {defaultImageStoreMaxFileBytes, minImageStoreMaxFileBytes, maxImageStoreMaxFileBytes},
		"max_total_bytes": {defaultImageStoreMaxTotalBytes, minImageStoreMaxTotalBytes, maxImageStoreMaxTotalBytes},
	} {
		property := properties[name]
		var def int64
		if err := json.Unmarshal(property.Default, &def); err != nil || def != limits[0] || property.Minimum == nil || *property.Minimum != limits[1] || property.Maximum == nil || *property.Maximum != limits[2] {
			t.Errorf("%s: schema default/min/max differ from the loader %v (schema default %s)", name, limits, property.Default)
		}
	}
}
