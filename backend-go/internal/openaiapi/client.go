package openaiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
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
			return &StatusError{Status: apiError.StatusCode, err: err}
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
		if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
			// The run was stopped during the wait: report the stop, not the earlier failure.
			return ssestream.NewStream[sdkresponses.ResponseStreamEventUnion](nil, sleepErr)
		}
	}
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
