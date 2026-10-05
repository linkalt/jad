package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultNativeFetchStorageDir            = ".fetch"
	defaultNativeFetchIncludeMetadata       = false
	defaultNativeFetchIncludeImages         = false
	defaultNativeFetchLimit           int64 = 12_000
	defaultNativeFetchMaxLimit        int64 = 50_000
	defaultNativeFetchResponseBytes   int64 = 2_000_000
	defaultNativeFetchDocumentChars   int64 = 1_000_000
	defaultNativeFetchStorageBytes    int64 = 64 * 1024 * 1024
	defaultNativeFetchTimeoutMS       int64 = 15_000
	defaultNativeFetchMaxRedirects    int64 = 5
	maxNativeFetchTimeoutMS           int64 = 300_000
	maxNativeFetchRedirects           int64 = 20
)

var nativeFetchHostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

// NativeFetchSettings contains normalized network, document, and storage limits
// for native.fetch.
type NativeFetchSettings struct {
	StorageDir       string
	DefaultLimit     int64
	MaxLimit         int64
	MaxResponseBytes int64
	MaxDocumentChars int64
	MaxStorageBytes  int64
	TimeoutMS        int64
	MaxRedirects     int64
	IncludeMetadata  bool
	IncludeImages    bool
	AllowedHosts     []string
	DeniedHosts      []string
}

func normalizeNativeFetchSettings(setting map[string]json.RawMessage, settingPath, cwd, toolStorageDir string) (*NativeFetchSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := toolFields["native"]
	if !exists {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	fetchRaw, exists := nativeFields["fetch"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(fetchRaw, settingPath+": tools.native.fetch")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		switch key {
		case "storage_dir", "default_limit", "max_limit", "max_response_bytes", "max_document_chars", "max_storage_bytes", "timeout_ms", "max_redirects", "include_metadata", "include_images", "allowed_hosts", "denied_hosts":
		default:
			return nil, configError("%s: tools.native.fetch.%s is not supported", settingPath, key)
		}
	}

	settings := &NativeFetchSettings{AllowedHosts: []string{}, DeniedHosts: []string{}}
	if settings.DefaultLimit, err = configuredInteger(fields, "default_limit", defaultNativeFetchLimit, 1, defaultNativeFetchDocumentChars, settingPath, "tools.native.fetch"); err != nil {
		return nil, err
	}
	if settings.MaxLimit, err = configuredInteger(fields, "max_limit", defaultNativeFetchMaxLimit, 1, defaultNativeFetchDocumentChars, settingPath, "tools.native.fetch"); err != nil {
		return nil, err
	}
	if settings.MaxResponseBytes, err = configuredInteger(fields, "max_response_bytes", defaultNativeFetchResponseBytes, 1, maxNativeToolBytes, settingPath, "tools.native.fetch"); err != nil {
		return nil, err
	}
	if settings.MaxDocumentChars, err = configuredInteger(fields, "max_document_chars", defaultNativeFetchDocumentChars, 1, maxNativeToolBytes, settingPath, "tools.native.fetch"); err != nil {
		return nil, err
	}
	if settings.MaxStorageBytes, err = configuredInteger(fields, "max_storage_bytes", defaultNativeFetchStorageBytes, 1, maxNativeToolBytes*100, settingPath, "tools.native.fetch"); err != nil {
		return nil, err
	}
	if settings.TimeoutMS, err = configuredInteger(fields, "timeout_ms", defaultNativeFetchTimeoutMS, 1, maxNativeFetchTimeoutMS, settingPath, "tools.native.fetch"); err != nil {
		return nil, err
	}
	if settings.MaxRedirects, err = configuredInteger(fields, "max_redirects", defaultNativeFetchMaxRedirects, 1, maxNativeFetchRedirects, settingPath, "tools.native.fetch"); err != nil {
		return nil, err
	}
	if settings.IncludeMetadata, err = nativeFetchBoolean(fields, "include_metadata", defaultNativeFetchIncludeMetadata, settingPath); err != nil {
		return nil, err
	}
	if settings.IncludeImages, err = nativeFetchBoolean(fields, "include_images", defaultNativeFetchIncludeImages, settingPath); err != nil {
		return nil, err
	}
	if settings.AllowedHosts, err = nativeFetchHosts(fields, "allowed_hosts", settingPath); err != nil {
		return nil, err
	}
	if settings.DeniedHosts, err = nativeFetchHosts(fields, "denied_hosts", settingPath); err != nil {
		return nil, err
	}
	settings.StorageDir, err = normalizeNativeFetchStorageDir(fields, cwd, toolStorageDir, settingPath)
	if err != nil {
		return nil, err
	}
	if settings.DefaultLimit > settings.MaxLimit || settings.MaxLimit > settings.MaxDocumentChars || settings.MaxDocumentChars > settings.MaxStorageBytes {
		return nil, configError("%s: tools.native.fetch limits are incompatible", settingPath)
	}
	return settings, nil
}

func nativeFetchBoolean(fields map[string]json.RawMessage, name string, fallback bool, settingPath string) (bool, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return fallback, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, configError("%s: tools.native.fetch.%s must be a boolean", settingPath, name)
	}
	return value, nil
}

func nativeFetchHosts(fields map[string]json.RawMessage, name, settingPath string) ([]string, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return []string{}, nil
	}
	var hosts []string
	if err := json.Unmarshal(raw, &hosts); err != nil || hosts == nil {
		return nil, configError("%s: tools.native.fetch.%s must contain host names or IP addresses", settingPath, name)
	}
	seen := make(map[string]struct{}, len(hosts))
	normalized := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(host)
		if !nativeFetchHostPattern.MatchString(host) {
			return nil, configError("%s: tools.native.fetch.%s must contain host names or IP addresses", settingPath, name)
		}
		if _, exists := seen[host]; exists {
			continue
		}
		seen[host] = struct{}{}
		normalized = append(normalized, host)
	}
	return normalized, nil
}

func normalizeNativeFetchStorageDir(fields map[string]json.RawMessage, cwd, toolStorageDir, settingPath string) (string, error) {
	configured := defaultNativeFetchStorageDir
	if raw, exists := fields["storage_dir"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return "", configError("%s: tools.native.fetch.storage_dir must be a non-empty string", settingPath)
		}
		configured = strings.TrimSpace(configured)
	}
	configured = expandHome(configured)
	if !filepath.IsAbs(configured) {
		base := cwd
		if toolStorageDir != "" {
			base = toolStorageDir
		}
		configured = filepath.Join(base, configured)
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve tools.native.fetch.storage_dir: %w", err)
	}
	return filepath.Clean(absolute), nil
}
