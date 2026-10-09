package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	defaultImageResizeMaxEdgePx    int64 = 1568
	minImageResizeMaxEdgePx        int64 = 64
	maxImageResizeMaxEdgePx        int64 = 8192
	defaultImageResizeMaxBytes     int64 = 1024 * 1024
	minImageResizeMaxBytes         int64 = 4 * 1024
	maxImageResizeMaxBytes         int64 = 20 * 1024 * 1024
	defaultImageResizeMaxPixels    int64 = 64_000_000
	minImageResizeMaxPixels        int64 = 4096
	maxImageResizeMaxPixels        int64 = 400_000_000
	defaultImageResizeJPEGQuality  int64 = 85
	minImageResizeJPEGQuality      int64 = 40
	maxImageResizeJPEGQuality      int64 = 100
	defaultImageResizeCacheEntries int64 = 64
	minImageResizeCacheEntries     int64 = 1
	maxImageResizeCacheEntries     int64 = 4096
)

var (
	imageResizeKeys = []string{"applies_to", "cache_entries", "enabled", "jpeg_quality", "max_bytes", "max_edge_px", "max_pixels"}
	// imageResizeSources are the places an image can come from: a tool result of an MCP
	// server, a tool result of a native tool, or a user message.
	imageResizeSources = []string{"mcp", "native", "user"}
)

// ImageResizeSettings controls the step that shrinks images between the transcript and the
// provider. It is off by default. With it off, or with no source to apply to, the request
// is exactly the one built without the setting.
type ImageResizeSettings struct {
	Enabled bool
	// AppliesTo lists the sources whose images are resized: "mcp", "native", "user".
	// The default is only "mcp".
	AppliesTo []string
	// MaxEdgePx is the longest side an image keeps.
	MaxEdgePx int64
	// MaxBytes is the largest encoded image sent as it is, and the size the result is held under.
	MaxBytes int64
	// MaxPixels is the largest decoded image (width x height) that is opened at all.
	MaxPixels int64
	// JPEGQuality is the first quality tried for an opaque image that does not fit as a png.
	JPEGQuality int64
	// CacheEntries is how many finished resizes are remembered.
	CacheEntries int64
}

// Active reports whether any image can be resized.
func (settings ImageResizeSettings) Active() bool {
	return settings.Enabled && len(settings.AppliesTo) > 0
}

// normalizeImageResizeSettings reads the `image_resize` node. Only an absent key takes a
// default; a null or misspelt value is an error, so a typo cannot quietly turn it off.
func normalizeImageResizeSettings(raw json.RawMessage, settingPath string) (ImageResizeSettings, error) {
	settings := ImageResizeSettings{
		AppliesTo:    []string{"mcp"},
		MaxEdgePx:    defaultImageResizeMaxEdgePx,
		MaxBytes:     defaultImageResizeMaxBytes,
		MaxPixels:    defaultImageResizeMaxPixels,
		JPEGQuality:  defaultImageResizeJPEGQuality,
		CacheEntries: defaultImageResizeCacheEntries,
	}
	if len(raw) == 0 {
		return settings, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": image_resize")
	if err != nil {
		return ImageResizeSettings{}, configError("%s: image_resize must be an object", settingPath)
	}
	for key := range fields {
		if !containsString(imageResizeKeys, key) {
			return ImageResizeSettings{}, unsupportedToolKey(settingPath+": image_resize", key, imageResizeKeys, "")
		}
	}
	if value, exists := fields["enabled"]; exists {
		var enabled bool
		if json.Unmarshal(value, &enabled) != nil || strings.TrimSpace(string(value)) == "null" {
			return ImageResizeSettings{}, configError("%s: image_resize.enabled must be a boolean", settingPath)
		}
		settings.Enabled = enabled
	}
	bounded := func(name string, target *int64, minimum, maximum int64) error {
		value, exists := fields[name]
		if !exists {
			return nil
		}
		configured, ok := configSafeInteger(value)
		if !ok || configured < minimum || configured > maximum {
			return configError("%s: image_resize.%s must be a safe integer between %d and %d", settingPath, name, minimum, maximum)
		}
		*target = configured
		return nil
	}
	for _, limit := range []struct {
		name             string
		target           *int64
		minimum, maximum int64
	}{
		{"max_edge_px", &settings.MaxEdgePx, minImageResizeMaxEdgePx, maxImageResizeMaxEdgePx},
		{"max_bytes", &settings.MaxBytes, minImageResizeMaxBytes, maxImageResizeMaxBytes},
		{"max_pixels", &settings.MaxPixels, minImageResizeMaxPixels, maxImageResizeMaxPixels},
		{"jpeg_quality", &settings.JPEGQuality, minImageResizeJPEGQuality, maxImageResizeJPEGQuality},
		{"cache_entries", &settings.CacheEntries, minImageResizeCacheEntries, maxImageResizeCacheEntries},
	} {
		if err := bounded(limit.name, limit.target, limit.minimum, limit.maximum); err != nil {
			return ImageResizeSettings{}, err
		}
	}
	if value, exists := fields["applies_to"]; exists {
		var sources []string
		if json.Unmarshal(value, &sources) != nil || sources == nil {
			return ImageResizeSettings{}, configError("%s: image_resize.applies_to must be a list of %s", settingPath, strings.Join(imageResizeSources, ", "))
		}
		seen := map[string]bool{}
		for _, source := range sources {
			if !containsString(imageResizeSources, source) || seen[source] {
				return ImageResizeSettings{}, configError("%s: image_resize.applies_to entry %q must be one of %s, each once", settingPath, source, strings.Join(imageResizeSources, ", "))
			}
			seen[source] = true
		}
		settings.AppliesTo = sources
	}
	return settings, nil
}
