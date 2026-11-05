package sessionfs

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Web search providers of tools.native.web_search.
const (
	WebSearchProviderBrave   = "brave"
	WebSearchProviderTavily  = "tavily"
	WebSearchProviderSearXNG = "searxng"
)

const (
	defaultNativeWebSearchResults         int64 = 5
	defaultNativeWebSearchMaxResults      int64 = 10
	maxNativeWebSearchMaxResults          int64 = 50
	defaultNativeWebSearchTimeoutMS       int64 = 15_000
	maxNativeWebSearchTimeoutMS           int64 = 120_000
	defaultNativeWebSearchResponseBytes   int64 = 2_000_000
	maxNativeWebSearchResponseBytes       int64 = 64_000_000
	defaultNativeWebSearchSnippetChars    int64 = 500
	maxNativeWebSearchSnippetChars        int64 = 20_000
	defaultNativeWebSearchMaxQueryChars   int64 = 400
	maxNativeWebSearchMaxQueryChars       int64 = 4_000
	defaultNativeWebSearchUserAgent             = "Mozilla/5.0 (compatible; jad-web-search/1.0)"
	maxNativeWebSearchDomains                   = 100
	defaultNativeWebSearchBraveBaseURL          = "https://api.search.brave.com/res/v1/web/search"
	defaultNativeWebSearchTavilyBaseURL         = "https://api.tavily.com/search"
	defaultNativeWebSearchBraveAPIKeyEnv        = "BRAVE_SEARCH_API_KEY"
	defaultNativeWebSearchTavilyAPIKeyEnv       = "TAVILY_API_KEY"
	nativeWebSearchSection                      = "tools.native.web_search"
	maxNativeWebSearchUserAgentChars            = 512
)

var (
	nativeWebSearchKeys          = []string{"provider", "base_url", "api_key_env", "default_results", "max_results", "timeout_ms", "max_response_bytes", "max_snippet_chars", "max_query_chars", "user_agent", "allowed_domains", "blocked_domains"}
	nativeWebSearchDomainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	nativeWebSearchEnvName       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// NativeWebSearchSettings are the normalized tools.native.web_search settings: a search
// API the model queries by text, for models whose provider runs no hosted web search.
type NativeWebSearchSettings struct {
	Provider string
	// BaseURL is the search endpoint: the provider's API by default, a SearXNG /search URL.
	BaseURL string
	// APIKeyEnv names the environment variable that holds the key; it is read on each
	// call. Empty for a provider that needs none (searxng).
	APIKeyEnv        string
	DefaultResults   int64
	MaxResults       int64
	TimeoutMS        int64
	MaxResponseBytes int64
	MaxSnippetChars  int64
	MaxQueryChars    int64
	UserAgent        string
	// AllowedDomains, when not empty, keeps only results on these domains (or their
	// subdomains); BlockedDomains drops results on them. A call may narrow both further.
	AllowedDomains []string
	BlockedDomains []string
}

func normalizeNativeWebSearchSettings(setting map[string]json.RawMessage, settingPath string) (*NativeWebSearchSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil || !exists {
		return nil, err
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["web_search"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": "+nativeWebSearchSection)
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if !slices.Contains(nativeWebSearchKeys, key) {
			return nil, configError("%s: %s.%s is not supported", settingPath, nativeWebSearchSection, key)
		}
	}
	prefix := settingPath + ": " + nativeWebSearchSection
	settings := &NativeWebSearchSettings{}
	if settings.Provider, err = bashString(fields, "provider", "", prefix, true); err != nil {
		return nil, err
	}
	defaultBaseURL, defaultKeyEnv := "", ""
	switch settings.Provider {
	case WebSearchProviderBrave:
		defaultBaseURL, defaultKeyEnv = defaultNativeWebSearchBraveBaseURL, defaultNativeWebSearchBraveAPIKeyEnv
	case WebSearchProviderTavily:
		defaultBaseURL, defaultKeyEnv = defaultNativeWebSearchTavilyBaseURL, defaultNativeWebSearchTavilyAPIKeyEnv
	case WebSearchProviderSearXNG:
	case "":
		return nil, configError("%s.provider is required: brave, tavily or searxng", prefix)
	default:
		return nil, configError("%s.provider must be brave, tavily or searxng", prefix)
	}
	if settings.BaseURL, err = bashString(fields, "base_url", defaultBaseURL, prefix, true); err != nil {
		return nil, err
	}
	settings.BaseURL = strings.TrimSpace(settings.BaseURL)
	if settings.BaseURL == "" {
		return nil, configError("%s.base_url is required for provider searxng (for example http://127.0.0.1:8888/search)", prefix)
	}
	if parsed, parseErr := url.Parse(settings.BaseURL); parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, configError("%s.base_url must be an http or https URL", prefix)
	}
	if settings.APIKeyEnv, err = bashString(fields, "api_key_env", defaultKeyEnv, prefix, true); err != nil {
		return nil, err
	}
	settings.APIKeyEnv = strings.TrimSpace(settings.APIKeyEnv)
	if settings.APIKeyEnv != "" && !nativeWebSearchEnvName.MatchString(settings.APIKeyEnv) {
		return nil, configError("%s.api_key_env must be an environment variable name", prefix)
	}
	if settings.Provider != WebSearchProviderSearXNG && settings.APIKeyEnv == "" {
		return nil, configError("%s.api_key_env must name the variable that holds the %s key", prefix, settings.Provider)
	}
	if settings.MaxResults, err = configuredInteger(fields, "max_results", defaultNativeWebSearchMaxResults, 1, maxNativeWebSearchMaxResults, settingPath, nativeWebSearchSection); err != nil {
		return nil, err
	}
	defaultResults := min(defaultNativeWebSearchResults, settings.MaxResults)
	if settings.DefaultResults, err = configuredInteger(fields, "default_results", defaultResults, 1, maxNativeWebSearchMaxResults, settingPath, nativeWebSearchSection); err != nil {
		return nil, err
	}
	if settings.DefaultResults > settings.MaxResults {
		return nil, configError("%s.default_results must not be larger than max_results", prefix)
	}
	if settings.TimeoutMS, err = configuredInteger(fields, "timeout_ms", defaultNativeWebSearchTimeoutMS, 1, maxNativeWebSearchTimeoutMS, settingPath, nativeWebSearchSection); err != nil {
		return nil, err
	}
	if settings.MaxResponseBytes, err = configuredInteger(fields, "max_response_bytes", defaultNativeWebSearchResponseBytes, 1024, maxNativeWebSearchResponseBytes, settingPath, nativeWebSearchSection); err != nil {
		return nil, err
	}
	if settings.MaxSnippetChars, err = configuredInteger(fields, "max_snippet_chars", defaultNativeWebSearchSnippetChars, 0, maxNativeWebSearchSnippetChars, settingPath, nativeWebSearchSection); err != nil {
		return nil, err
	}
	if settings.MaxQueryChars, err = configuredInteger(fields, "max_query_chars", defaultNativeWebSearchMaxQueryChars, 1, maxNativeWebSearchMaxQueryChars, settingPath, nativeWebSearchSection); err != nil {
		return nil, err
	}
	if settings.UserAgent, err = bashString(fields, "user_agent", defaultNativeWebSearchUserAgent, prefix, false); err != nil {
		return nil, err
	}
	if len(settings.UserAgent) > maxNativeWebSearchUserAgentChars || strings.ContainsAny(settings.UserAgent, "\r\n") {
		return nil, configError("%s.user_agent must be one line of at most %d characters", prefix, maxNativeWebSearchUserAgentChars)
	}
	if settings.AllowedDomains, err = nativeWebSearchDomains(fields, "allowed_domains", prefix); err != nil {
		return nil, err
	}
	if settings.BlockedDomains, err = nativeWebSearchDomains(fields, "blocked_domains", prefix); err != nil {
		return nil, err
	}
	return settings, nil
}

// nativeWebSearchDomains reads a list of domain names, lower-cased and without repeats.
func nativeWebSearchDomains(fields map[string]json.RawMessage, name, prefix string) ([]string, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return []string{}, nil
	}
	var domains []string
	if err := json.Unmarshal(raw, &domains); err != nil || domains == nil || len(domains) > maxNativeWebSearchDomains {
		return nil, configError("%s.%s must be a list of at most %d domain names", prefix, name, maxNativeWebSearchDomains)
	}
	normalized, ok := NormalizeWebSearchDomains(domains)
	if !ok {
		return nil, configError("%s.%s must be a list of at most %d domain names", prefix, name, maxNativeWebSearchDomains)
	}
	return normalized, nil
}

// NormalizeWebSearchDomains lower-cases domain names, drops a leading "www." and
// repeats, and reports false when one is not a domain name.
func NormalizeWebSearchDomains(domains []string) ([]string, bool) {
	normalized := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), "www.")
		if !nativeWebSearchDomainPattern.MatchString(domain) {
			return nil, false
		}
		if !slices.Contains(normalized, domain) {
			normalized = append(normalized, domain)
		}
	}
	return normalized, true
}
