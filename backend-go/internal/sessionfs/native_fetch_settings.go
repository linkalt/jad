package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	defaultNativeFetchStorageDir              = ".fetch"
	defaultNativeFetchIncludeMetadata         = false
	defaultNativeFetchIncludeImages           = false
	defaultNativeFetchUserAgent               = "Mozilla/5.0 (compatible; jad-fetch/1.0)"
	defaultNativeFetchTruncateOversized       = false
	defaultNativeFetchCacheTTLMS        int64 = 0
	maxNativeFetchUserAgentChars              = 512
	maxNativeFetchCacheTTLMS            int64 = 86_400_000
	defaultNativeFetchLimit             int64 = 12_000
	defaultNativeFetchMaxLimit          int64 = 50_000
	defaultNativeFetchResponseBytes     int64 = 2_000_000
	defaultNativeFetchDocumentChars     int64 = 1_000_000
	defaultNativeFetchStorageBytes      int64 = 64 * 1024 * 1024
	defaultNativeFetchTimeoutMS         int64 = 15_000
	defaultNativeFetchMaxRedirects      int64 = 5
	defaultNativeFetchKeepAliveMS       int64 = 30_000
	defaultNativeFetchKeepAliveHosts    int64 = 8
	maxNativeFetchTimeoutMS             int64 = 300_000
	maxNativeFetchKeepAliveHosts        int64 = 64
	maxNativeFetchRedirects             int64 = 20
)

var nativeFetchHostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

var nativeFetchKeys = []string{"storage_dir", "default_limit", "max_limit", "max_response_bytes", "max_document_chars", "max_storage_bytes", "timeout_ms", "max_redirects", "keep_alive_ms", "max_keep_alive_hosts", "include_metadata", "include_images", "user_agent", "truncate_oversized_response", "cache_ttl_ms", "allowed_hosts", "denied_hosts"}

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
	// KeepAliveMS is how long an idle connection to a fetched host stays open for
	// the next fetch; 0 opens a fresh connection per request.
	KeepAliveMS int64
	// MaxKeepAliveHosts bounds how many hosts keep a connection open at once.
	MaxKeepAliveHosts int64
	IncludeMetadata   bool
	IncludeImages     bool
	UserAgent         string
	// TruncateOversizedResponse stores the first MaxResponseBytes of a larger
	// response instead of failing.
	TruncateOversizedResponse bool
	// CacheTTLMS is how long a fetched URL is served from its stored document; 0 disables.
	CacheTTLMS   int64
	AllowedHosts []string
	DeniedHosts  []string
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
		if !slices.Contains(nativeFetchKeys, key) {
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
	if settings.KeepAliveMS, err = configuredInteger(fields, "keep_alive_ms", defaultNativeFetchKeepAliveMS, 0, maxNativeFetchTimeoutMS, settingPath, "tools.native.fetch"); err != nil {
		return nil, err
	}
	if settings.MaxKeepAliveHosts, err = configuredInteger(fields, "max_keep_alive_hosts", defaultNativeFetchKeepAliveHosts, 1, maxNativeFetchKeepAliveHosts, settingPath, "tools.native.fetch"); err != nil {
		return nil, err
	}
	if settings.IncludeMetadata, err = nativeFetchBoolean(fields, "include_metadata", defaultNativeFetchIncludeMetadata, settingPath); err != nil {
		return nil, err
	}
	if settings.IncludeImages, err = nativeFetchBoolean(fields, "include_images", defaultNativeFetchIncludeImages, settingPath); err != nil {
		return nil, err
	}
	if settings.UserAgent, err = nativeFetchUserAgent(fields, settingPath); err != nil {
		return nil, err
	}
	if settings.TruncateOversizedResponse, err = nativeFetchBoolean(fields, "truncate_oversized_response", defaultNativeFetchTruncateOversized, settingPath); err != nil {
		return nil, err
	}
	if settings.CacheTTLMS, err = configuredInteger(fields, "cache_ttl_ms", defaultNativeFetchCacheTTLMS, 0, maxNativeFetchCacheTTLMS, settingPath, "tools.native.fetch"); err != nil {
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

func nativeFetchUserAgent(fields map[string]json.RawMessage, settingPath string) (string, error) {
	raw, exists := fields["user_agent"]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return defaultNativeFetchUserAgent, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", configError("%s: tools.native.fetch.user_agent must be a string of 1 to %d characters without control characters", settingPath, maxNativeFetchUserAgentChars)
	}
	value = strings.TrimSpace(value)
	count := utf8.RuneCountInString(value)
	if count < 1 || count > maxNativeFetchUserAgentChars || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", configError("%s: tools.native.fetch.user_agent must be a string of 1 to %d characters without control characters", settingPath, maxNativeFetchUserAgentChars)
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
