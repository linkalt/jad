package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestImageResizeDefaultsToOff(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	want := ImageResizeSettings{AppliesTo: []string{"mcp"}, MaxEdgePx: 1568, MaxBytes: 1 << 20, MaxPixels: 64_000_000, JPEGQuality: 85, CacheEntries: 64}
	if !reflect.DeepEqual(config.ImageResize, want) || config.ImageResize.Active() {
		t.Fatalf("default image_resize = %+v, want %+v and inactive", config.ImageResize, want)
	}
}

func TestImageResizeEnabledAloneKeepsEveryDefault(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"image_resize": map[string]any{"enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !config.ImageResize.Active() || !reflect.DeepEqual(config.ImageResize.AppliesTo, []string{"mcp"}) || config.ImageResize.MaxEdgePx != 1568 {
		t.Fatalf("image_resize = %+v", config.ImageResize)
	}
}

func TestImageResizeAcceptsEveryKey(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"image_resize": map[string]any{
		"enabled": true, "applies_to": []any{"user", "mcp", "native"}, "max_edge_px": 1024, "max_bytes": 204800,
		"max_pixels": 1000000, "jpeg_quality": 70, "cache_entries": 8,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := ImageResizeSettings{Enabled: true, AppliesTo: []string{"user", "mcp", "native"}, MaxEdgePx: 1024, MaxBytes: 204800, MaxPixels: 1_000_000, JPEGQuality: 70, CacheEntries: 8}
	if !reflect.DeepEqual(config.ImageResize, want) {
		t.Fatalf("image_resize = %+v, want %+v", config.ImageResize, want)
	}
}

func TestImageResizeEmptySourceListIsAllowedAndInactive(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"image_resize": map[string]any{"enabled": true, "applies_to": []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if config.ImageResize.Active() {
		t.Fatalf("an empty applies_to resizes nothing, so the setting must be inactive: %+v", config.ImageResize)
	}
}

func TestImageResizeRejectsBadValues(t *testing.T) {
	for _, test := range []struct {
		name        string
		imageResize any
		want        string
	}{
		{"not an object", "on", "image_resize must be an object"},
		{"array", []any{}, "image_resize must be an object"},
		{"null", nil, "image_resize must be an object"},
		{"misspelt key", map[string]any{"enable": true}, "image_resize.enable is not supported; did you mean enabled?"},
		{"enabled string", map[string]any{"enabled": "yes"}, "image_resize.enabled must be a boolean"},
		{"enabled null", map[string]any{"enabled": nil}, "image_resize.enabled must be a boolean"},
		{"edge low", map[string]any{"max_edge_px": 63}, "image_resize.max_edge_px must be a safe integer between 64 and 8192"},
		{"edge high", map[string]any{"max_edge_px": 8193}, "image_resize.max_edge_px must be a safe integer between 64 and 8192"},
		{"edge float", map[string]any{"max_edge_px": 100.5}, "image_resize.max_edge_px must be a safe integer"},
		{"edge string", map[string]any{"max_edge_px": "100"}, "image_resize.max_edge_px must be a safe integer"},
		{"bytes low", map[string]any{"max_bytes": 4095}, "image_resize.max_bytes must be a safe integer between 4096 and 20971520"},
		{"bytes high", map[string]any{"max_bytes": 20971521}, "image_resize.max_bytes must be a safe integer between 4096 and 20971520"},
		{"pixels low", map[string]any{"max_pixels": 4095}, "image_resize.max_pixels must be a safe integer between 4096 and 400000000"},
		{"quality low", map[string]any{"jpeg_quality": 39}, "image_resize.jpeg_quality must be a safe integer between 40 and 100"},
		{"quality high", map[string]any{"jpeg_quality": 101}, "image_resize.jpeg_quality must be a safe integer between 40 and 100"},
		{"cache zero", map[string]any{"cache_entries": 0}, "image_resize.cache_entries must be a safe integer between 1 and 4096"},
		{"sources not a list", map[string]any{"applies_to": "mcp"}, "image_resize.applies_to must be a list of mcp, native, user"},
		{"sources null", map[string]any{"applies_to": nil}, "image_resize.applies_to must be a list of mcp, native, user"},
		{"unknown source", map[string]any{"applies_to": []any{"web"}}, `image_resize.applies_to entry "web" must be one of mcp, native, user, each once`},
		{"duplicate source", map[string]any{"applies_to": []any{"mcp", "mcp"}}, `image_resize.applies_to entry "mcp" must be one of mcp, native, user, each once`},
		{"source number", map[string]any{"applies_to": []any{1}}, "image_resize.applies_to must be a list of mcp, native, user"},
	} {
		_, err := loadApprovalSetting(t, map[string]any{"image_resize": test.imageResize})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err = %v, want it to contain %q", test.name, err, test.want)
		}
	}
}

func TestImageResizeSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			ImageResize struct {
				AdditionalProperties bool `json:"additionalProperties"`
				Properties           map[string]struct {
					Type    string          `json:"type"`
					Default json.RawMessage `json:"default"`
					Minimum *int64          `json:"minimum"`
					Maximum *int64          `json:"maximum"`
				} `json:"properties"`
			} `json:"image_resize"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties.ImageResize.Properties
	var keys []string
	for key := range properties {
		keys = append(keys, key)
	}
	want := append([]string(nil), imageResizeKeys...)
	sort.Strings(keys)
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) || schema.Properties.ImageResize.AdditionalProperties {
		t.Fatalf("schema keys %v (additionalProperties %v), loader accepts %v", keys, schema.Properties.ImageResize.AdditionalProperties, want)
	}
	// The documented defaults and limits are the ones the loader uses.
	for name, limits := range map[string][3]int64{
		"max_edge_px":   {defaultImageResizeMaxEdgePx, minImageResizeMaxEdgePx, maxImageResizeMaxEdgePx},
		"max_bytes":     {defaultImageResizeMaxBytes, minImageResizeMaxBytes, maxImageResizeMaxBytes},
		"max_pixels":    {defaultImageResizeMaxPixels, minImageResizeMaxPixels, maxImageResizeMaxPixels},
		"jpeg_quality":  {defaultImageResizeJPEGQuality, minImageResizeJPEGQuality, maxImageResizeJPEGQuality},
		"cache_entries": {defaultImageResizeCacheEntries, minImageResizeCacheEntries, maxImageResizeCacheEntries},
	} {
		property := properties[name]
		var def int64
		if err := json.Unmarshal(property.Default, &def); err != nil || def != limits[0] || property.Minimum == nil || *property.Minimum != limits[1] || property.Maximum == nil || *property.Maximum != limits[2] {
			t.Errorf("%s: schema default/min/max differ from the loader %v (schema default %s)", name, limits, property.Default)
		}
	}
	var full struct {
		Properties struct {
			ImageResize struct {
				Properties struct {
					AppliesTo struct {
						Items struct {
							Enum []string `json:"enum"`
						} `json:"items"`
						Default []string `json:"default"`
					} `json:"applies_to"`
				} `json:"properties"`
			} `json:"image_resize"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &full); err != nil {
		t.Fatal(err)
	}
	applies := full.Properties.ImageResize.Properties.AppliesTo
	if !reflect.DeepEqual(applies.Items.Enum, imageResizeSources) || !reflect.DeepEqual(applies.Default, []string{"mcp"}) {
		t.Fatalf("schema applies_to enum %v default %v, loader %v and [mcp]", applies.Items.Enum, applies.Default, imageResizeSources)
	}
}
