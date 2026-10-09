package openaiapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	streamresponses "example.com/agent-harness/backend-go/internal/responses"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	sdkresponses "github.com/openai/openai-go/v3/responses"
)

// Config configures the OpenAI-compatible Responses API client.
type Config struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	// Retry, when set, replaces the SDK's own retries for the request that opens a
	// response stream. When nil the SDK's defaults apply, as before the setting existed.
	Retry *RetryConfig
}

// Event and EventHandler remain aliases for the provider-neutral stream
// contract so callers of this adapter do not depend on SDK-specific types.
type Event = streamresponses.Event

type EventHandler = streamresponses.EventHandler

// Client adapts the OpenAI Go SDK's Responses streaming API to raw JSON events.
type Client struct {
	sdk   *openai.Client
	retry *RetryConfig
	// sleep and random are replaced by tests so a retry wait costs no time.
	sleep  func(context.Context, time.Duration) error
	random func() float64
}

// NewClient creates a client. An empty APIKey leaves credential lookup to the
// SDK's standard OPENAI_API_KEY environment-variable behavior; an empty BaseURL
// uses the SDK default.
func NewClient(config Config) *Client {
	options := make([]option.RequestOption, 0, 3)
	if config.APIKey != "" {
		options = append(options, option.WithAPIKey(config.APIKey))
	}
	if config.BaseURL != "" {
		options = append(options, option.WithBaseURL(config.BaseURL))
	}
	if config.HTTPClient != nil {
		options = append(options, option.WithHTTPClient(config.HTTPClient))
	}
	sdk := openai.NewClient(options...)
	return &Client{sdk: &sdk, retry: config.Retry, sleep: sleepContext, random: defaultRandom}
}

// Stream posts a JSON Responses request and forwards each server-sent event
// without decoding/re-encoding its payload. The SDK supplies authentication,
// HTTP transport, and SSE framing; request fields are injected as raw JSON so
// provider-specific and future request fields remain available.
func (c *Client) Stream(ctx context.Context, request json.RawMessage, handle EventHandler) error {
	if c == nil || c.sdk == nil {
		return errors.New("OpenAI client is not initialized")
	}
	if handle == nil {
		return errors.New("OpenAI stream event handler is required")
	}
	requestOptions, err := rawRequestOptions(request)
	if err != nil {
		return err
	}

	stream := c.openStream(ctx, requestOptions)
	defer func() { _ = stream.Close() }()

	for stream.Next() {
		current := stream.Current()
		raw := json.RawMessage(current.RawJSON())
		if !json.Valid(raw) {
			return fmt.Errorf("OpenAI stream event %q did not contain valid raw JSON", current.Type)
		}
		if err := handle(Event{Type: current.Type, Raw: raw}); err != nil {
			return fmt.Errorf("handle OpenAI stream event %q: %w", current.Type, err)
		}
	}
	if err := stream.Err(); err != nil {
		err = fmt.Errorf("read OpenAI Responses stream: %w", err)
		var apiError *openai.Error
		if errors.As(err, &apiError) && apiError.StatusCode > 0 {
			return &StatusError{Status: apiError.StatusCode, err: withResponseBody(err, apiError)}
		}
		return err
	}
	return nil
}

// openStream opens the response stream. With a retry setting, a failed opening request
// is retried here; the SDK's own retries are switched off so this loop is the only one.
// The SDK records an opening failure in the stream before any event is read, so
// stream.Err() right after NewStreaming separates "the request failed" from "the stream
// broke later": only the first is retried, because after that partial output was shown.
func (c *Client) openStream(ctx context.Context, requestOptions []option.RequestOption) *ssestream.Stream[sdkresponses.ResponseStreamEventUnion] {
	if c.retry == nil {
		return c.sdk.Responses.NewStreaming(ctx, sdkresponses.ResponseNewParams{}, requestOptions...)
	}
	options := append(append([]option.RequestOption{}, requestOptions...), option.WithMaxRetries(0))
	notify := streamresponses.RetryNotifierFrom(ctx)
	reported := false
	// The opening request is over on every return below, whether it worked, failed for
	// good or was stopped, so a wait that was announced is always cleared.
	defer func() {
		if reported {
			notify(nil)
		}
	}()
	for attempt := 0; ; attempt++ {
		stream := c.sdk.Responses.NewStreaming(ctx, sdkresponses.ResponseNewParams{}, options...)
		err := stream.Err()
		if err == nil || attempt >= c.retry.MaxRetries || ctx.Err() != nil || !isRetryableError(err) {
			return stream
		}
		_ = stream.Close()
		delay := retryDelay(*c.retry, attempt, statusOf(err), responseHeader(err), time.Now(), c.random)
		if c.retry.OnRetry != nil {
			c.retry.OnRetry(RetryEvent{Attempt: attempt + 1, MaxRetries: c.retry.MaxRetries, Delay: delay, Status: statusOf(err)})
		}
		if notify != nil {
			reported = true
			notify(&streamresponses.RetryWait{Attempt: attempt + 1, MaxRetries: c.retry.MaxRetries, Status: statusOf(err), Delay: delay})
		}
		if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
			// The run was stopped during the wait: report the stop, not the earlier failure.
			return ssestream.NewStream[sdkresponses.ResponseStreamEventUnion](nil, sleepErr)
		}
	}
}

// bodyError is a failure whose message also carries the provider's response body.
// Unwrap keeps the SDK's *openai.Error reachable for errors.As.
type bodyError struct {
	err  error
	body string
}

func (e *bodyError) Error() string { return strings.TrimRight(e.err.Error(), " ") + " " + e.body }
func (e *bodyError) Unwrap() error { return e.err }

// withResponseBody returns err with the provider's response body added when the SDK's
// own message left it out. The SDK reads the body of a failed response, keeps only the
// value of a top-level "error" key and prints that; a provider that answers in another
// shape, such as {"detail": "..."} or plain text, ends up with an empty message tail
// ("400 Bad Request "). The SDK puts the body back on Response.Body, so it is read here
// from that copy and put back again for any later reader. Nothing is added when the SDK
// already printed a body, so a standard error is unchanged.
//
// Since openai-go v3.74 the SDK's message is only "OpenAI API error: <status>" and no
// longer carries the body, even for the standard shape, so the error object it parsed
// (RawJSON) is added here. The text reaches the server log only; the client gets the
// status alone (see StatusError).
func withResponseBody(err error, apiError *openai.Error) error {
	if raw := strings.TrimSpace(apiError.RawJSON()); raw != "" {
		return &bodyError{err: err, body: raw}
	}
	if apiError.Response == nil || apiError.Response.Body == nil {
		return err
	}
	body, readErr := io.ReadAll(apiError.Response.Body)
	apiError.Response.Body = io.NopCloser(bytes.NewReader(body))
	if readErr != nil {
		return err
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return err
	}
	var compact bytes.Buffer
	if json.Valid(body) && json.Compact(&compact, body) == nil {
		text = compact.String()
	}
	return &bodyError{err: err, body: text}
}

// StatusError is a failure the provider answered with an HTTP status. The
// session reports the status, and only the status, to the client; the response
// body stays in the server log.
type StatusError struct {
	Status int
	err    error
}

func (e *StatusError) Error() string { return e.err.Error() }
func (e *StatusError) Unwrap() error { return e.err }

// ProviderStatus is the HTTP status the provider returned.
func (e *StatusError) ProviderStatus() int { return e.Status }

func rawRequestOptions(request json.RawMessage) ([]option.RequestOption, error) {
	if !json.Valid(request) {
		return nil, errors.New("Responses request must be valid JSON")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(request, &fields); err != nil {
		return nil, fmt.Errorf("Responses request must be a JSON object: %w", err)
	}
	if fields == nil {
		return nil, errors.New("Responses request must be a JSON object")
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	options := make([]option.RequestOption, 0, len(keys))
	for _, key := range keys {
		options = append(options, option.WithJSONSet(key, fields[key]))
	}
	return options, nil
}
