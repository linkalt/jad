package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// WebSearchToolID is the id of the web search tool definition.
const WebSearchToolID = "extension.web_search"

// braveMaxCount is the most results the Brave web search API returns for one request.
const braveMaxCount = 20

var WebSearchInputSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"description":"What to search the web for."},"max_results":{"type":"integer","minimum":1,"description":"How many results to return; the setting's max_results caps it."},"allowed_domains":{"type":"array","items":{"type":"string","minLength":1},"description":"Only return results from these domains (and their subdomains)."},"blocked_domains":{"type":"array","items":{"type":"string","minLength":1},"description":"Never return results from these domains."}},"required":["query"],"additionalProperties":false}`)

// webSearchResult is one search hit as the tool shows it.
type webSearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// webSearchQuery is one validated call.
type webSearchQuery struct {
	Text    string
	Count   int
	Allowed []string
	Blocked []string
}

// NewWebSearchTool constructs extension.web_search: it sends the query to the configured
// search API and returns titles, URLs and snippets. The key is read from the environment
// on each call.
func NewWebSearchTool(settings sessionfs.ExtensionWebSearchSettings) (tools.Tool, error) {
	return newWebSearchTool(settings, os.Getenv, &http.Client{})
}

func newWebSearchTool(settings sessionfs.ExtensionWebSearchSettings, environment func(string) string, client *http.Client) (tools.Tool, error) {
	validator, err := tools.CompileSchema(WebSearchInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile extension web_search schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          WebSearchToolID,
			Source:      "extension",
			Name:        "web_search",
			Description: "Search the web and get result titles, URLs and snippets. Use fetch to read a result.",
			InputSchema: append(json.RawMessage(nil), WebSearchInputSchema...),
		},
		Validator:        validator,
		ReadOnly:         true,
		ExampleOperation: json.RawMessage(`{"query":"golang context cancellation"}`),
		LeafBudget:       true,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeWebSearch(ctx, arguments, settings, environment, client, execution.CallID), nil
		},
	}, nil
}

func executeWebSearch(ctx context.Context, arguments map[string]any, settings sessionfs.ExtensionWebSearchSettings, environment func(string) string, client *http.Client, callID string) string {
	query, problem := parseWebSearchArguments(arguments, settings)
	if problem != "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", problem)
	}
	key := ""
	if settings.APIKeyEnv != "" {
		key = strings.TrimSpace(environment(settings.APIKeyEnv))
		if key == "" && settings.Provider != sessionfs.WebSearchProviderSearXNG {
			return tools.ExecutionError(callID, tools.StatusError, "api_key_missing", "Environment variable "+settings.APIKeyEnv+" is not set (tools.extension.web_search.api_key_env)")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutMS)*time.Millisecond)
	defer cancel()
	request, err := webSearchRequest(ctx, settings, query, key)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "search_failed", "Could not build the search request: "+err.Error())
	}
	request.Header.Set("User-Agent", settings.UserAgent)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return tools.ExecutionError(callID, tools.StatusError, "search_timeout", fmt.Sprintf("The search did not answer within %d ms (tools.extension.web_search.timeout_ms)", settings.TimeoutMS))
		}
		if ctx.Err() != nil {
			return tools.ExecutionError(callID, tools.StatusError, "search_cancelled", "The search was cancelled")
		}
		return tools.ExecutionError(callID, tools.StatusError, "search_failed", "The search request failed: "+err.Error())
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, settings.MaxResponseBytes+1))
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "search_failed", "Could not read the search response: "+err.Error())
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return tools.ExecutionError(callID, tools.StatusError, "search_failed", fmt.Sprintf("The search API answered HTTP %d", response.StatusCode),
			tools.Information{Key: "response", Value: webSearchCut(strings.TrimSpace(string(body)), 500)})
	}
	if int64(len(body)) > settings.MaxResponseBytes {
		return tools.ExecutionError(callID, tools.StatusError, "response_too_large", fmt.Sprintf("The search response is larger than %d bytes (tools.extension.web_search.max_response_bytes)", settings.MaxResponseBytes))
	}
	results, err := decodeWebSearchResults(settings.Provider, body)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "search_failed", "The search response could not be read: "+err.Error())
	}
	results = filterWebSearchResults(results, query.Allowed, query.Blocked)
	if len(results) > query.Count {
		results = results[:query.Count]
	}
	rendered, err := tools.RenderToolResult(tools.ResultOptions{
		ID:      callID,
		Status:  tools.StatusOK,
		Summary: fmt.Sprintf("%d results for %q", len(results), query.Text),
		Information: []tools.Information{
			{Key: "provider", Value: settings.Provider},
			{Key: "results", Value: len(results)},
		},
		Content: &tools.Content{Text: renderWebSearchResults(results, int(settings.MaxSnippetChars))},
	})
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "render_failed", err.Error())
	}
	return rendered
}

func parseWebSearchArguments(arguments map[string]any, settings sessionfs.ExtensionWebSearchSettings) (webSearchQuery, string) {
	query := webSearchQuery{Count: int(settings.DefaultResults)}
	text, _ := arguments["query"].(string)
	query.Text = strings.Join(strings.Fields(text), " ")
	if query.Text == "" {
		return query, "query must not be blank"
	}
	if utf8.RuneCountInString(query.Text) > int(settings.MaxQueryChars) {
		return query, fmt.Sprintf("query is longer than %d characters (tools.extension.web_search.max_query_chars)", settings.MaxQueryChars)
	}
	if value, exists := arguments["max_results"]; exists {
		count, ok := tools.SafeInteger(value)
		if !ok || count < 1 {
			return query, "max_results must be a positive integer"
		}
		query.Count = int(min(count, settings.MaxResults))
	}
	var ok bool
	if query.Allowed, ok = webSearchDomainArgument(arguments["allowed_domains"]); !ok {
		return query, "allowed_domains must be a list of domain names such as example.com"
	}
	if query.Blocked, ok = webSearchDomainArgument(arguments["blocked_domains"]); !ok {
		return query, "blocked_domains must be a list of domain names such as example.com"
	}
	// The setting's lists apply to every call: a call may narrow the allowed domains to
	// a subset, never widen them, and adds to the blocked ones.
	if len(settings.AllowedDomains) > 0 {
		if len(query.Allowed) == 0 {
			query.Allowed = append([]string(nil), settings.AllowedDomains...)
		} else {
			narrowed := make([]string, 0, len(query.Allowed))
			for _, domain := range query.Allowed {
				if domainMatches(domain, settings.AllowedDomains) {
					narrowed = append(narrowed, domain)
				}
			}
			if len(narrowed) == 0 {
				return query, "allowed_domains names no domain the setting allows: " + strings.Join(settings.AllowedDomains, ", ")
			}
			query.Allowed = narrowed
		}
	}
	query.Blocked = append(query.Blocked, settings.BlockedDomains...)
	return query, ""
}

func webSearchDomainArgument(value any) ([]string, bool) {
	if value == nil {
		return nil, true
	}
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	domains := make([]string, 0, len(list))
	for _, item := range list {
		domain, ok := item.(string)
		if !ok {
			return nil, false
		}
		domains = append(domains, domain)
	}
	return sessionfs.NormalizeWebSearchDomains(domains)
}

// webSearchRequest builds the provider's HTTP request.
func webSearchRequest(ctx context.Context, settings sessionfs.ExtensionWebSearchSettings, query webSearchQuery, key string) (*http.Request, error) {
	switch settings.Provider {
	case sessionfs.WebSearchProviderTavily:
		body, err := json.Marshal(map[string]any{
			"query":           query.Text,
			"max_results":     query.Count,
			"include_domains": nonNil(query.Allowed),
			"exclude_domains": nonNil(query.Blocked),
		})
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.BaseURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+key)
		return request, nil
	case sessionfs.WebSearchProviderBrave, sessionfs.WebSearchProviderSearXNG:
		endpoint, err := url.Parse(settings.BaseURL)
		if err != nil {
			return nil, err
		}
		values := endpoint.Query()
		values.Set("q", withSiteFilter(query.Text, query.Allowed))
		if settings.Provider == sessionfs.WebSearchProviderBrave {
			values.Set("count", strconv.Itoa(min(query.Count, braveMaxCount)))
		} else {
			values.Set("format", "json")
		}
		endpoint.RawQuery = values.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		switch {
		case settings.Provider == sessionfs.WebSearchProviderBrave:
			request.Header.Set("X-Subscription-Token", key)
		case key != "":
			request.Header.Set("Authorization", "Bearer "+key)
		}
		return request, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", settings.Provider)
	}
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// withSiteFilter adds site: operators for the allowed domains, so a provider without a
// domain parameter searches only them; the results are filtered again afterwards.
func withSiteFilter(text string, allowed []string) string {
	if len(allowed) == 0 {
		return text
	}
	sites := make([]string, len(allowed))
	for index, domain := range allowed {
		sites[index] = "site:" + domain
	}
	return text + " (" + strings.Join(sites, " OR ") + ")"
}

// decodeWebSearchResults reads the results of a provider's JSON answer.
func decodeWebSearchResults(provider string, body []byte) ([]webSearchResult, error) {
	type hit struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	var hits []hit
	switch provider {
	case sessionfs.WebSearchProviderBrave:
		var answer struct {
			Web struct {
				Results []hit `json:"results"`
			} `json:"web"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			return nil, err
		}
		hits = answer.Web.Results
	default:
		var answer struct {
			Results []hit `json:"results"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			return nil, err
		}
		hits = answer.Results
	}
	results := make([]webSearchResult, 0, len(hits))
	seen := make(map[string]struct{}, len(hits))
	for _, hit := range hits {
		link := strings.TrimSpace(hit.URL)
		if link == "" {
			continue
		}
		if _, repeated := seen[link]; repeated {
			continue
		}
		seen[link] = struct{}{}
		snippet := hit.Description
		if snippet == "" {
			snippet = hit.Content
		}
		results = append(results, webSearchResult{Title: cleanSearchText(hit.Title), URL: link, Snippet: cleanSearchText(snippet)})
	}
	return results, nil
}

// cleanSearchText removes the highlighting tags search APIs put in titles and snippets
// and collapses whitespace.
func cleanSearchText(text string) string {
	for _, tag := range []string{"<strong>", "</strong>", "<b>", "</b>", "<em>", "</em>"} {
		text = strings.ReplaceAll(text, tag, "")
	}
	for _, entity := range [][2]string{{"&quot;", `"`}, {"&#39;", "'"}, {"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}} {
		text = strings.ReplaceAll(text, entity[0], entity[1])
	}
	return strings.Join(strings.Fields(text), " ")
}

// filterWebSearchResults keeps results on an allowed domain (when any are named) and
// drops those on a blocked one.
func filterWebSearchResults(results []webSearchResult, allowed, blocked []string) []webSearchResult {
	kept := results[:0:0]
	for _, result := range results {
		parsed, err := url.Parse(result.URL)
		if err != nil || parsed.Hostname() == "" {
			continue
		}
		host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
		if len(allowed) > 0 && !domainMatches(host, allowed) {
			continue
		}
		if domainMatches(host, blocked) {
			continue
		}
		kept = append(kept, result)
	}
	return kept
}

// domainMatches reports whether host is one of domains or a subdomain of one.
func domainMatches(host string, domains []string) bool {
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func renderWebSearchResults(results []webSearchResult, maxSnippetChars int) string {
	if len(results) == 0 {
		return "No results."
	}
	var out strings.Builder
	for index, result := range results {
		if index > 0 {
			out.WriteString("\n")
		}
		title := result.Title
		if title == "" {
			title = result.URL
		}
		fmt.Fprintf(&out, "%d. %s\n   %s\n", index+1, title, result.URL)
		if snippet := webSearchCut(result.Snippet, maxSnippetChars); snippet != "" && maxSnippetChars > 0 {
			fmt.Fprintf(&out, "   %s\n", snippet)
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

// webSearchCut keeps the first limit characters of text, adding "…" when it cut.
func webSearchCut(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…"
}
