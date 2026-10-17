package native

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	brotli "github.com/andybalholm/brotli"
	htmltomarkdown "github.com/xberg-io/html-to-markdown/packages/go/v3"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

//go:generate go run github.com/xberg-io/html-to-markdown/packages/go/v3/cmd/setup -dir .

const fetchToolID = "native.fetch"

var fetchInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "url":{"type":"string","minLength":1,"description":"Public http or https URL to fetch when cursor is absent or no longer available."},
    "cursor":{"type":"string","minLength":1,"description":"Cursor returned by a previous fetch call. A valid cursor takes precedence over url."},
    "limit":{"type":"integer","minimum":1,"description":"Optional character limit for this page."}
  },
  "additionalProperties":false,
  "anyOf":[
    {"properties":{"url":{}},"required":["url"]},
    {"properties":{"cursor":{}},"required":["cursor"]}
  ]
}`)

var (
	fetchTitlePattern = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	fetchTagPattern   = regexp.MustCompile(`<[^>]+>`)
)

var knownTextApplicationTypes = map[string]struct{}{
	"application/markdown":              {},
	"application/x-markdown":            {},
	"application/json":                  {},
	"application/xml":                   {},
	"application/yaml":                  {},
	"application/x-yaml":                {},
	"application/toml":                  {},
	"application/x-toml":                {},
	"application/javascript":            {},
	"application/x-javascript":          {},
	"application/ecmascript":            {},
	"application/sql":                   {},
	"application/graphql":               {},
	"application/ndjson":                {},
	"application/x-ndjson":              {},
	"application/rtf":                   {},
	"application/x-www-form-urlencoded": {},
}

// fetchIPResolver is intentionally small so URL validation is deterministic in
// tests without replacing process-wide DNS behavior.
type fetchIPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// fetchRequester connects only to the address chosen by validateFetchURL while
// keeping the original URL hostname for HTTP Host and TLS verification.
type fetchRequester interface {
	Do(context.Context, validatedFetchURL) (*http.Response, error)
}

type validatedFetchURL struct {
	URL     *url.URL
	Address net.IPAddr
}

// pinnedFetchRequester reuses connections through pool when keepAlive is positive;
// a zero value opens a fresh connection for every request.
type pinnedFetchRequester struct {
	pool      *fetchTransportPool
	keepAlive time.Duration
	maxHosts  int
}

type fetchRequestError struct {
	Code    string
	Message string
}

func (e *fetchRequestError) Error() string { return e.Message }

// NewFetchTool constructs native.fetch with its normalized settings and store.
func NewFetchTool(settings sessionfs.NativeFetchSettings, store *FetchStore) (tools.Tool, error) {
	if store == nil {
		store = NewFetchStore(settings.StorageDir)
	}
	return newFetchTool(settings, store, net.DefaultResolver, newPinnedFetchRequester(settings, store))
}

func newPinnedFetchRequester(settings sessionfs.NativeFetchSettings, store *FetchStore) pinnedFetchRequester {
	return pinnedFetchRequester{pool: &store.transports, keepAlive: time.Duration(settings.KeepAliveMS) * time.Millisecond, maxHosts: int(settings.MaxKeepAliveHosts)}
}

func newFetchTool(settings sessionfs.NativeFetchSettings, store *FetchStore, resolver fetchIPResolver, requester fetchRequester) (tools.Tool, error) {
	validator, err := tools.CompileSchema(fetchInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native fetch schema: %w", err)
	}
	if store == nil {
		store = NewFetchStore(settings.StorageDir)
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if requester == nil {
		requester = pinnedFetchRequester{}
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          fetchToolID,
			Source:      "native",
			Name:        "fetch",
			Description: "Fetch a public text or HTML page; HTML is converted to Markdown and other text is returned as-is. Use cursor to continue.",
			InputSchema: append(json.RawMessage(nil), fetchInputSchema...),
		},
		Validator:          validator,
		ReadOnly:           true,
		ExampleOperation:   json.RawMessage(`{"url":"https://example.com"}`),
		NormalizeArguments: normalizeFetchArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeFetch(ctx, arguments, settings, store, resolver, requester, execution)
		},
	}, nil
}

func normalizeFetchArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	for _, key := range []string{"url", "cursor"} {
		if value, ok := normalized[key].(string); ok {
			normalized[key] = strings.TrimSpace(value)
		}
	}
	if limit, ok := safeInteger(normalized["limit"]); ok {
		if limit < 1 {
			limit = 1
		}
		normalized["limit"] = json.Number(strconv.FormatInt(limit, 10))
	}
	return normalized
}

func executeNativeFetch(ctx context.Context, arguments map[string]any, settings sessionfs.NativeFetchSettings, store *FetchStore, resolver fetchIPResolver, requester fetchRequester, execution tools.ExecutionContext) (string, error) {
	for key := range arguments {
		if key != "url" && key != "cursor" && key != "limit" {
			return fetchExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_input", "Fetch accepts url, cursor, and limit"), nil
		}
	}
	urlValue, _ := arguments["url"].(string)
	cursor, _ := arguments["cursor"].(string)
	urlValue = strings.TrimSpace(urlValue)
	cursor = strings.TrimSpace(cursor)
	if urlValue == "" && cursor == "" {
		return fetchExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_input", "Fetch requires url or cursor"), nil
	}
	requested := settings.DefaultLimit
	if value, exists := arguments["limit"]; exists {
		limit, ok := safeInteger(value)
		if !ok {
			return fetchExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_limit", "limit must be a positive integer"), nil
		}
		requested = limit
	}
	if requested < 1 {
		return fetchExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_limit", "limit must be a positive integer"), nil
	}
	limit := requested
	if limit > settings.MaxLimit {
		limit = settings.MaxLimit
	}

	if cursor != "" {
		page, err := store.Page(cursor, limit)
		if err == nil {
			return renderFetchPage(execution.CallID, page)
		}
		var storeError *FetchStoreError
		if !errors.As(err, &storeError) {
			return fetchExecutionError(execution.CallID, tools.StatusError, "fetch_failed", err.Error()), nil
		}
		if urlValue == "" {
			return fetchExecutionError(execution.CallID, tools.StatusError, "fetch_storage_error", storeError.Error()), nil
		}
	}

	document, err := retrieveFetchDocument(ctx, urlValue, settings, resolver, requester)
	if err != nil {
		var requestError *fetchRequestError
		if errors.As(err, &requestError) {
			status := tools.StatusError
			if requestError.Code == "invalid_url" {
				status = tools.StatusInvalidInput
			}
			return fetchExecutionError(execution.CallID, status, requestError.Code, requestError.Message), nil
		}
		return fetchExecutionError(execution.CallID, tools.StatusError, "fetch_failed", err.Error()), nil
	}
	page, err := store.CreatePage(document, limit, settings)
	if err != nil {
		var storeError *FetchStoreError
		if errors.As(err, &storeError) {
			return fetchExecutionError(execution.CallID, tools.StatusError, "fetch_storage_error", storeError.Error()), nil
		}
		return fetchExecutionError(execution.CallID, tools.StatusError, "fetch_failed", err.Error()), nil
	}
	return renderFetchPage(execution.CallID, page)
}

func renderFetchPage(callID string, page FetchPage) (string, error) {
	information := []tools.Information{
		{Key: "url", Value: page.Document.URL},
		{Key: "final_url", Value: page.Document.FinalURL},
		{Key: "status", Value: page.Document.Status},
		{Key: "content_type", Value: page.Document.ContentType},
	}
	if page.Document.Title != "" {
		information = append(information, tools.Information{Key: "title", Value: page.Document.Title})
	}
	if page.NextCursor != "" {
		information = append(information, tools.Information{Key: "next_cursor", Value: page.NextCursor})
	}
	summary := "Document fetched"
	if page.NextCursor != "" {
		summary = "Document fetched; more content is available"
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID: callID, Status: tools.StatusOK, Summary: summary,
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: page.Content},
	})
}

func fetchExecutionError(callID string, status tools.Status, code, message string) string {
	return tools.ExecutionError(callID, status, code, message)
}

func retrieveFetchDocument(ctx context.Context, urlValue string, settings sessionfs.NativeFetchSettings, resolver fetchIPResolver, requester fetchRequester) (FetchDocument, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestContext, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutMS)*time.Millisecond)
	defer cancel()

	nextURL := urlValue
	for redirects := int64(0); ; redirects++ {
		if err := requestContext.Err(); err != nil {
			return FetchDocument{}, fetchTimeoutError()
		}
		target, err := validateFetchURL(requestContext, nextURL, settings, resolver)
		if err != nil {
			return FetchDocument{}, err
		}
		response, err := requester.Do(requestContext, target)
		if err != nil {
			if requestContext.Err() != nil {
				return FetchDocument{}, fetchTimeoutError()
			}
			return FetchDocument{}, &fetchRequestError{Code: "request_failed", Message: "web request failed"}
		}
		if response == nil {
			return FetchDocument{}, &fetchRequestError{Code: "request_failed", Message: "web request failed"}
		}
		if requestContext.Err() != nil {
			closeFetchResponse(response)
			return FetchDocument{}, fetchTimeoutError()
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			if redirects >= settings.MaxRedirects {
				closeFetchResponse(response)
				return FetchDocument{}, &fetchRequestError{Code: "too_many_redirects", Message: "fetch exceeded configured redirect limit"}
			}
			location := response.Header.Get("Location")
			closeFetchResponse(response)
			if location == "" {
				return FetchDocument{}, &fetchRequestError{Code: "invalid_redirect", Message: "redirect response has no location"}
			}
			resolved, err := target.URL.Parse(location)
			if err != nil {
				return FetchDocument{}, &fetchRequestError{Code: "invalid_redirect", Message: "redirect response has an invalid location"}
			}
			nextURL = resolved.String()
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			closeFetchResponse(response)
			return FetchDocument{}, &fetchRequestError{Code: "http_error", Message: fmt.Sprintf("web request returned HTTP %d", response.StatusCode)}
		}
		contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0]))
		if !isSupportedFetchContentType(contentType) {
			closeFetchResponse(response)
			return FetchDocument{}, &fetchRequestError{Code: "unsupported_content_type", Message: "response content type is not a supported text format"}
		}
		body, err := readFetchResponse(response, settings.MaxResponseBytes)
		if err != nil {
			if requestContext.Err() != nil {
				return FetchDocument{}, fetchTimeoutError()
			}
			var requestError *fetchRequestError
			if errors.As(err, &requestError) {
				return FetchDocument{}, requestError
			}
			return FetchDocument{}, &fetchRequestError{Code: "request_failed", Message: "web request failed"}
		}
		body = []byte(strings.ToValidUTF8(string(body), "\uFFFD"))
		isHTML := contentType == "text/html" || contentType == "application/xhtml+xml"
		markdown := string(body)
		if isHTML {
			converted, err := htmltomarkdown.Convert(string(body), &htmltomarkdown.ConversionOptions{
				ExtractMetadata:          htmltomarkdown.Ptr(settings.IncludeMetadata),
				SkipImages:               !settings.IncludeImages,
				ExtractImages:            false,
				IncludeDocumentStructure: false,
			})
			if err != nil {
				return FetchDocument{}, &fetchRequestError{Code: "request_failed", Message: "web request failed"}
			}
			if converted != nil && converted.Content != nil {
				markdown = *converted.Content
			} else {
				markdown = ""
			}
		}
		if int64(utf16Length(markdown)) > settings.MaxDocumentChars {
			return FetchDocument{}, &fetchRequestError{Code: "document_too_large", Message: "document exceeds configured character limit"}
		}
		pageTitle := ""
		if isHTML {
			pageTitle = fetchHTMLTitle(string(body))
		}
		return FetchDocument{
			URL: urlValue, FinalURL: target.URL.String(), Title: pageTitle,
			ContentType: contentType, Status: response.StatusCode, Markdown: markdown,
		}, nil
	}
}

func validateFetchURL(ctx context.Context, value string, settings sessionfs.NativeFetchSettings, resolver fetchIPResolver) (validatedFetchURL, error) {
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return validatedFetchURL{}, &fetchRequestError{Code: "invalid_url", Message: "url must be an absolute http or https URL"}
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return validatedFetchURL{}, &fetchRequestError{Code: "invalid_url", Message: "url must use http or https"}
	}
	if parsed.User != nil {
		return validatedFetchURL{}, &fetchRequestError{Code: "invalid_url", Message: "url must not include credentials"}
	}
	hostname := parsed.Hostname()
	if hostname == "" || strings.Contains(hostname, "%") {
		return validatedFetchURL{}, &fetchRequestError{Code: "invalid_url", Message: "url must be an absolute http or https URL"}
	}
	host := strings.ToLower(hostname)
	if containsHost(settings.DeniedHosts, host) {
		return validatedFetchURL{}, &fetchRequestError{Code: "host_denied", Message: "URL host is denied by fetch policy"}
	}
	if len(settings.AllowedHosts) > 0 && !containsHost(settings.AllowedHosts, host) {
		return validatedFetchURL{}, &fetchRequestError{Code: "host_denied", Message: "URL host is not allowed by fetch policy"}
	}
	if err := ctx.Err(); err != nil {
		return validatedFetchURL{}, fetchTimeoutError()
	}
	var addresses []net.IPAddr
	if ip := net.ParseIP(hostname); ip != nil {
		addresses = []net.IPAddr{{IP: ip}}
	} else {
		addresses, err = resolver.LookupIPAddr(ctx, hostname)
		if err != nil {
			if ctx.Err() != nil {
				return validatedFetchURL{}, fetchTimeoutError()
			}
			return validatedFetchURL{}, &fetchRequestError{Code: "unresolvable_host", Message: "URL host could not be resolved"}
		}
	}
	if len(addresses) == 0 {
		return validatedFetchURL{}, &fetchRequestError{Code: "unresolvable_host", Message: "URL host could not be resolved"}
	}
	for _, address := range addresses {
		if !isPublicFetchAddress(address) {
			return validatedFetchURL{}, &fetchRequestError{Code: "unsafe_url", Message: "URL resolves to a private or reserved address"}
		}
	}
	selected := addresses[0]
	if selected.IP == nil || (selected.IP.To4() == nil && selected.IP.To16() == nil) {
		return validatedFetchURL{}, &fetchRequestError{Code: "unsafe_url", Message: "URL host returned an invalid address"}
	}
	parsed.Scheme = scheme
	parsed.Host = canonicalFetchHost(hostname, parsed.Port(), scheme)
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return validatedFetchURL{URL: parsed, Address: selected}, nil
}

func canonicalFetchHost(hostname, port, scheme string) string {
	host := strings.ToLower(hostname)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "80" && scheme == "http" || port == "443" && scheme == "https" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	return host
}

func containsHost(hosts []string, host string) bool {
	for _, candidate := range hosts {
		if strings.EqualFold(candidate, host) {
			return true
		}
	}
	return false
}

func isPublicFetchAddress(address net.IPAddr) bool {
	ip := address.IP
	if ip == nil || address.Zone != "" || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		first, second := int(v4[0]), int(v4[1])
		switch {
		case first == 0 || first == 10 || first == 127 || first >= 224:
			return false
		case first == 100 && second >= 64 && second <= 127:
			return false
		case first == 169 && second == 254:
			return false
		case first == 172 && second >= 16 && second <= 31:
			return false
		case first == 192 && (second == 0 || second == 168):
			return false
		case first == 198 && (second == 18 || second == 19):
			return false
		}
		return true
	}
	if ip.To16() == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return false
	}
	if ip[0]&0xfe == 0xfc || ip[0] == 0xfe && ip[1]&0xc0 == 0x80 {
		return false
	}
	// Reject IPv4-compatible and documentation prefixes in addition to the
	// private/link-local ranges excluded above.
	if ip[0] == 0 && ip[1] == 0 && ip[2] == 0 && ip[3] == 0 && ip[4] == 0 && ip[5] == 0 {
		return false
	}
	if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return false
	}
	// Site-local (deprecated, but still treated as internal by many networks).
	if ip[0] == 0xfe && ip[1]&0xc0 == 0xc0 {
		return false
	}
	// NAT64 (64:ff9b::/96) and 6to4 (2002::/16) carry an IPv4 address that the network maps
	// back onto the IPv4 side, so it must be public as well.
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b && allZero(ip[4:12]) {
		return isPublicFetchAddress(net.IPAddr{IP: net.IP(append([]byte(nil), ip[12:16]...))})
	}
	if ip[0] == 0x20 && ip[1] == 0x02 {
		return isPublicFetchAddress(net.IPAddr{IP: net.IP(append([]byte(nil), ip[2:6]...))})
	}
	// Local-use NAT64 prefix (RFC 8215): its embedded address is not reachable from outside.
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b && ip[4] == 0x00 && ip[5] == 0x01 {
		return false
	}
	return true
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func (r pinnedFetchRequester) Do(ctx context.Context, target validatedFetchURL) (*http.Response, error) {
	port := target.URL.Port()
	if port == "" {
		if target.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	keepAlive := time.Duration(0)
	var transport *http.Transport
	pooled := r.pool != nil && r.keepAlive > 0
	if pooled {
		keepAlive = r.keepAlive
		key := fetchTransportKey{scheme: target.URL.Scheme, host: target.URL.Hostname(), port: port, ip: target.Address.IP.String()}
		transport = r.pool.get(key, keepAlive, r.maxHosts, func() *http.Transport {
			return newPinnedFetchTransport(target, port, keepAlive)
		})
	}
	if transport == nil {
		pooled = false
		transport = newPinnedFetchTransport(target, port, 0)
	}
	release := func() {}
	if !pooled {
		release = transport.CloseIdleConnections
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL.String(), nil)
	if err != nil {
		release()
		return nil, err
	}
	request.Header.Set("Accept", "text/*, application/*")
	request.Header.Set("Accept-Encoding", "gzip, deflate, br")
	response, err := client.Do(request)
	if err != nil {
		release()
		return nil, err
	}
	response.Body = &fetchResponseBody{ReadCloser: response.Body, closeIdle: release}
	return response, nil
}

type fetchResponseBody struct {
	io.ReadCloser
	closeIdle func()
}

func (b *fetchResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.closeIdle != nil {
		b.closeIdle()
	}
	return err
}

func readFetchResponse(response *http.Response, maximum int64) ([]byte, error) {
	defer closeFetchResponse(response)
	if response.Body == nil {
		return []byte{}, nil
	}
	if maximum < 1 {
		maximum = 1
	}
	encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
	if encoding == "" || encoding == "identity" {
		if declared, err := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64); err == nil && declared > maximum {
			return nil, &fetchRequestError{Code: "response_too_large", Message: "response exceeds configured byte limit"}
		}
	}
	var body io.Reader = response.Body
	var decoder io.Closer
	switch encoding {
	case "", "identity":
	case "gzip":
		reader, err := gzip.NewReader(response.Body)
		if err != nil {
			return nil, err
		}
		body, decoder = reader, reader
	case "deflate":
		reader, err := zlib.NewReader(response.Body)
		if err != nil {
			return nil, err
		}
		body, decoder = reader, reader
	case "br":
		body = brotli.NewReader(response.Body)
	default:
		return nil, &fetchRequestError{Code: "unsupported_content_encoding", Message: "response content encoding is not supported"}
	}
	if decoder != nil {
		defer decoder.Close()
	}
	limited := io.LimitReader(body, maximum+1)
	contents, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maximum {
		return nil, &fetchRequestError{Code: "response_too_large", Message: "response exceeds configured byte limit"}
	}
	return contents, nil
}

func closeFetchResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

func isSupportedFetchContentType(contentType string) bool {
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	if _, exists := knownTextApplicationTypes[contentType]; exists {
		return true
	}
	if !strings.HasPrefix(contentType, "application/") || strings.Count(contentType, "/") != 1 {
		return false
	}
	name := strings.TrimPrefix(contentType, "application/")
	return strings.HasSuffix(name, "+json") || strings.HasSuffix(name, "+xml")
}

func fetchHTMLTitle(html string) string {
	match := fetchTitlePattern.FindStringSubmatch(html)
	if len(match) < 2 {
		return ""
	}
	plain := fetchTagPattern.ReplaceAllString(match[1], " ")
	return strings.Join(strings.Fields(plain), " ")
}

func fetchTimeoutError() error {
	return &fetchRequestError{Code: "timeout", Message: "fetch timed out or was cancelled"}
}
