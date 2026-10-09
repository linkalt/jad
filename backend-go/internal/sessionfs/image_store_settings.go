package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

const (
	defaultImageStoreMinBytes      int64 = 16 * 1024
	minImageStoreMinBytes          int64 = 1
	maxImageStoreMinBytes          int64 = 64 * 1024 * 1024
	defaultImageStoreMaxFileBytes  int64 = 20 * 1024 * 1024
	minImageStoreMaxFileBytes      int64 = 1024
	maxImageStoreMaxFileBytes      int64 = 100 * 1024 * 1024
	defaultImageStoreMaxTotalBytes int64 = 1024 * 1024 * 1024
	minImageStoreMaxTotalBytes     int64 = 1024
	maxImageStoreMaxTotalBytes     int64 = 1 << 40
)

var imageStoreKeys = []string{"dir", "max_file_bytes", "max_total_bytes", "min_bytes"}

// ImageStoreSettings controls where large tool-result images are kept. It is off unless Dir
// is set; with it off, messages.jsonl is written exactly as before.
type ImageStoreSettings struct {
	// Dir is the absolute directory of the files. A relative setting is resolved against
	// the session directory. Empty means off.
	Dir string
	// MinBytes: a decoded image smaller than this stays inline in the transcript.
	MinBytes int64
	// MaxFileBytes: a decoded image larger than this stays inline.
	MaxFileBytes int64
	// MaxTotalBytes: when the directory holds this much, new images stay inline.
	MaxTotalBytes int64
}

// Active reports whether images are stored.
func (settings ImageStoreSettings) Active() bool { return settings.Dir != "" }

// normalizeImageStoreSettings reads the `image_store` node. Only an absent key takes a
// default; a null or misspelt value is an error.
func normalizeImageStoreSettings(raw json.RawMessage, sessionDir, settingPath string) (ImageStoreSettings, error) {
	settings := ImageStoreSettings{
		MinBytes:      defaultImageStoreMinBytes,
		MaxFileBytes:  defaultImageStoreMaxFileBytes,
		MaxTotalBytes: defaultImageStoreMaxTotalBytes,
	}
	if len(raw) == 0 {
		return settings, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": image_store")
	if err != nil {
		return ImageStoreSettings{}, configError("%s: image_store must be an object", settingPath)
	}
	for key := range fields {
		if !containsString(imageStoreKeys, key) {
			return ImageStoreSettings{}, unsupportedToolKey(settingPath+": image_store", key, imageStoreKeys, "")
		}
	}
	if value, exists := fields["dir"]; exists {
		var dir string
		if json.Unmarshal(value, &dir) != nil || strings.TrimSpace(dir) == "" || strings.ContainsRune(dir, 0) {
			return ImageStoreSettings{}, configError("%s: image_store.dir must be a non-empty path", settingPath)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(sessionDir, dir)
		}
		settings.Dir = filepath.Clean(dir)
	}
	for _, limit := range []struct {
		name             string
		target           *int64
		minimum, maximum int64
	}{
		{"min_bytes", &settings.MinBytes, minImageStoreMinBytes, maxImageStoreMinBytes},
		{"max_file_bytes", &settings.MaxFileBytes, minImageStoreMaxFileBytes, maxImageStoreMaxFileBytes},
		{"max_total_bytes", &settings.MaxTotalBytes, minImageStoreMaxTotalBytes, maxImageStoreMaxTotalBytes},
	} {
		value, exists := fields[limit.name]
		if !exists {
			continue
		}
		var configured int64
		if json.Unmarshal(value, &configured) != nil || configured < limit.minimum || configured > limit.maximum {
			return ImageStoreSettings{}, configError("%s: image_store.%s must be a safe integer between %d and %d", settingPath, limit.name, limit.minimum, limit.maximum)
		}
		*limit.target = configured
	}
	if settings.MinBytes > settings.MaxFileBytes {
		return ImageStoreSettings{}, configError("%s: image_store.min_bytes (%d) must not be above max_file_bytes (%d)", settingPath, settings.MinBytes, settings.MaxFileBytes)
	}
	if settings.MaxFileBytes > settings.MaxTotalBytes {
		return ImageStoreSettings{}, configError("%s: image_store.max_file_bytes (%d) must not be above max_total_bytes (%d)", settingPath, settings.MaxFileBytes, settings.MaxTotalBytes)
	}
	return settings, nil
}
