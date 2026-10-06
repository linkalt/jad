package openaiapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/openai/openai-go/v3"
)

func apiError(status int, headers map[string]string) error {
	response := &http.Response{StatusCode: status, Header: http.Header{}}
	for key, value := range headers {
		response.Header.Set(key, value)
	}
	return &openai.Error{StatusCode: status, Response: response}
}

func TestIsRetryableError(t *testing.T) {
	for _, status := range []int{408, 409, 429, 500, 502, 503, 529} {
		if !isRetryableError(apiError(status, nil)) {
			t.Errorf("status %d should be retried", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 422} {
		if isRetryableError(apiError(status, nil)) {
			t.Errorf("status %d should not be retried", status)
		}
	}
	if isRetryableError(apiError(500, map[string]string{"x-should-retry": "false"})) {
		t.Error("x-should-retry false must stop a 500")
	}
	if !isRetryableError(apiError(400, map[string]string{"x-should-retry": "true"})) {
		t.Error("x-should-retry true must retry a 400")
	}
	if !isRetryableError(fmt.Errorf("wrapped: %w", apiError(429, nil))) {
		t.Error("a wrapped provider error is still retried")
	}
}

func TestIsRetryableErrorTransportAndStops(t *testing.T) {
	if isRetryableError(nil) {
		t.Error("nil is not an error")
	}
	if isRetryableError(context.Canceled) {
		t.Error("a stopped run must not be retried")
	}
	if isRetryableError(errors.New("a bug in this process")) {
		t.Error("a plain error is not a provider failure")
	}
	if !isRetryableError(io.ErrUnexpectedEOF) {
		t.Error("a connection cut before the response is retried")
	}
	if !isRetryableError(&netTimeout{}) {
		t.Error("a network timeout is retried")
	}
}

type netTimeout struct{}

func (*netTimeout) Error() string   { return "i/o timeout" }
func (*netTimeout) Timeout() bool   { return true }
func (*netTimeout) Temporary() bool { return true }

func TestRetryAfterReadsMillisecondsSecondsAndDates(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		header map[string]string
		want   time.Duration
		ok     bool
	}{
		{"ms wins over seconds", map[string]string{"Retry-After-Ms": "250", "Retry-After": "9"}, 250 * time.Millisecond, true},
		{"seconds", map[string]string{"Retry-After": "2"}, 2 * time.Second, true},
		{"fractional seconds", map[string]string{"Retry-After": "1.5"}, 1500 * time.Millisecond, true},
		{"http date in the future", map[string]string{"Retry-After": now.Add(3 * time.Second).Format(http.TimeFormat)}, 3 * time.Second, true},
		{"http date in the past", map[string]string{"Retry-After": now.Add(-3 * time.Second).Format(http.TimeFormat)}, 0, true},
		{"unreadable", map[string]string{"Retry-After": "soon"}, 0, false},
		{"negative ms", map[string]string{"Retry-After-Ms": "-5"}, 0, false},
		{"absent", nil, 0, false},
	}
	for _, tc := range cases {
		header := http.Header{}
		for key, value := range tc.header {
			header.Set(key, value)
		}
		got, ok := retryAfter(header, now)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRetryDelayBacksOffAndHonoursRetryAfterWithinTheCap(t *testing.T) {
	config := RetryConfig{MaxRetries: 2, InitialDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second}
	now := time.Now()
	none := func() float64 { return 0 }
	for attempt, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second} {
		if got := retryDelay(config, attempt, 0, nil, now, none); got != want {
			t.Errorf("attempt %d: got %v, want %v", attempt, got, want)
		}
	}
	if got := retryDelay(config, 4, 0, nil, now, none); got != 8*time.Second {
		t.Errorf("the backoff stops at the maximum, got %v", got)
	}
	if got := retryDelay(config, 9, 0, nil, now, none); got != 8*time.Second {
		t.Errorf("a huge attempt number still stops at the maximum, got %v", got)
	}
	if got := retryDelay(config, 1, 0, nil, now, func() float64 { return 1 }); got != 750*time.Millisecond {
		t.Errorf("the longest jitter takes a quarter off, got %v", got)
	}
	header := http.Header{"Retry-After-Ms": {"40"}}
	if got := retryDelay(config, 3, 0, header, now, none); got != 40*time.Millisecond {
		t.Errorf("Retry-After replaces the backoff, got %v", got)
	}
	header = http.Header{"Retry-After": {"120"}}
	if got := retryDelay(config, 0, 0, header, now, none); got != 8*time.Second {
		t.Errorf("Retry-After is capped to the maximum, got %v", got)
	}
	capped := RetryConfig{InitialDelay: time.Millisecond, MaxDelay: time.Second}
	if got := retryDelay(capped, 0, 0, http.Header{"Retry-After": {"5"}}, now, none); got != time.Second {
		t.Errorf("a configured cap applies to Retry-After, got %v", got)
	}
	for i := 0; i < 100; i++ {
		got := retryDelay(config, 2, 0, nil, now, defaultRandom)
		if got < 1500*time.Millisecond || got > 2*time.Second {
			t.Fatalf("jittered delay %v is outside [1.5s, 2s]", got)
		}
	}
}

func TestRateLimitResetReadsAnEpochOnA429Only(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	epochMs := func(offset time.Duration) string { return strconv.FormatInt(now.Add(offset).UnixMilli(), 10) }
	epochS := func(offset time.Duration) string { return strconv.FormatInt(now.Add(offset).Unix(), 10) }
	cases := []struct {
		name   string
		status int
		value  string
		want   time.Duration
		ok     bool
	}{
		{"epoch milliseconds", 429, epochMs(3 * time.Second), 3 * time.Second, true},
		{"epoch seconds", 429, epochS(3 * time.Second), 3 * time.Second, true},
		{"epoch milliseconds with a fraction", 429, epochMs(1500*time.Millisecond) + ".0", 1500 * time.Millisecond, true},
		{"already passed", 429, epochMs(-3 * time.Second), 0, false},
		{"now exactly", 429, epochMs(0), 0, false},
		{"a delta in seconds", 429, "30", 0, false},
		{"a delta in milliseconds below the floor", 429, "60000", 0, false},
		{"a duration string", 429, "6m0s", 0, false},
		{"text", 429, "soon", 0, false},
		{"negative", 429, "-" + epochMs(time.Second), 0, false},
		{"exponent", 429, "1.8e12", 0, false},
		{"hex", 429, "0x1", 0, false},
		{"empty", 429, "", 0, false},
		{"a 503 is ignored", 503, epochMs(3 * time.Second), 0, false},
		{"a transport failure is ignored", 0, epochMs(3 * time.Second), 0, false},
	}
	for _, tc := range cases {
		header := http.Header{}
		if tc.value != "" {
			header.Set("X-RateLimit-Reset", tc.value)
		}
		got, ok := rateLimitReset(header, tc.status, now)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRetryDelayFollowsTheResetWithinTheCapAndYieldsToRetryAfter(t *testing.T) {
	config := RetryConfig{MaxRetries: 2, InitialDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	none := func() float64 { return 0 }
	reset := func(offset time.Duration) http.Header {
		return http.Header{"X-Ratelimit-Reset": {strconv.FormatInt(now.Add(offset).UnixMilli(), 10)}}
	}
	if got := retryDelay(config, 0, 429, reset(3*time.Second), now, none); got != 3*time.Second {
		t.Errorf("the reset replaces the backoff on a 429, got %v", got)
	}
	if got := retryDelay(config, 0, 429, reset(45*time.Second), now, none); got != 8*time.Second {
		t.Errorf("the reset is capped to the maximum, got %v", got)
	}
	longer := RetryConfig{InitialDelay: 500 * time.Millisecond, MaxDelay: time.Minute}
	if got := retryDelay(longer, 0, 429, reset(45*time.Second), now, none); got != 45*time.Second {
		t.Errorf("a higher maximum lets the wait outlast the window, got %v", got)
	}
	if got := retryDelay(config, 0, 503, reset(3*time.Second), now, none); got != 500*time.Millisecond {
		t.Errorf("a 503 keeps the backoff, got %v", got)
	}
	both := reset(3 * time.Second)
	both.Set("Retry-After-Ms", "40")
	if got := retryDelay(config, 0, 429, both, now, none); got != 40*time.Millisecond {
		t.Errorf("Retry-After wins over the reset, got %v", got)
	}
	if got := retryDelay(config, 1, 429, reset(-time.Second), now, none); got != time.Second {
		t.Errorf("a reset in the past keeps the backoff, got %v", got)
	}
}

// providerServer answers each request with the next scripted reply: a status (with
// headers) or a streamed completion. It counts the requests it saw.
type providerReply struct {
	status  int
	headers map[string]string
	// partial streams one event and then closes the connection before response.completed.
	partial bool
}

func providerServer(t *testing.T, replies ...providerReply) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(count.Add(1)) - 1
		reply := replies[min(index, len(replies)-1)]
		if reply.status != 0 {
			for key, value := range reply.headers {
				w.Header().Set(key, value)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, `{"error":{"message":"scripted","type":"scripted"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		w.(http.Flusher).Flush()
		if reply.partial {
			// Drop the connection mid-stream: the client has already received an event.
			hijacker := w.(http.Hijacker)
			conn, _, _ := hijacker.Hijack()
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	t.Cleanup(server.Close)
	return server, &count
}

func retryClient(server *httptest.Server, retry *RetryConfig) (*Client, *[]time.Duration) {
	client := NewClient(Config{APIKey: "test-api-key", BaseURL: server.URL + "/v1/", Retry: retry})
	waits := &[]time.Duration{}
	client.sleep = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	}
	client.random = func() float64 { return 0 }
	return client, waits
}

func stream(client *Client, ctx context.Context) ([]string, error) {
	var events []string
	err := client.Stream(ctx, []byte(`{"model":"m","input":"hi"}`), func(event Event) error {
		events = append(events, event.Type)
		return nil
	})
	return events, err
}

func TestStreamRetriesAnOpeningFailureUntilItSucceeds(t *testing.T) {
	server, requests := providerServer(t, providerReply{status: 429}, providerReply{status: 500}, providerReply{})
	client, waits := retryClient(server, &RetryConfig{MaxRetries: 3, InitialDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second})
	events, err := stream(client, context.Background())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := strings.Join(events, ","); got != "response.created,response.completed" {
		t.Fatalf("events = %s", got)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("provider requests = %d, want 3 (the SDK's own retries must be off)", got)
	}
	if len(*waits) != 2 || (*waits)[0] != 500*time.Millisecond || (*waits)[1] != time.Second {
		t.Fatalf("waits = %v, want [500ms 1s]", *waits)
	}
}

func TestStreamReportsTheLastStatusOnceRetriesAreUsedUp(t *testing.T) {
	server, requests := providerServer(t, providerReply{status: 503})
	client, _ := retryClient(server, &RetryConfig{MaxRetries: 3, InitialDelay: time.Millisecond, MaxDelay: time.Second})
	_, err := stream(client, context.Background())
	var statusError *StatusError
	if !errors.As(err, &statusError) || statusError.ProviderStatus() != 503 {
		t.Fatalf("err = %v, want a StatusError with status 503", err)
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("provider requests = %d, want 4 (1 + 3 retries)", got)
	}
}

func TestStreamWithZeroRetriesMakesOneRequest(t *testing.T) {
	server, requests := providerServer(t, providerReply{status: 429})
	client, waits := retryClient(server, &RetryConfig{MaxRetries: 0, InitialDelay: time.Millisecond, MaxDelay: time.Second})
	if _, err := stream(client, context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if requests.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("requests = %d, waits = %v; want 1 request and no wait", requests.Load(), *waits)
	}
}

func TestStreamDoesNotRetryAStatusThatCannotSucceed(t *testing.T) {
	for _, status := range []int{400, 401, 404} {
		server, requests := providerServer(t, providerReply{status: status})
		client, _ := retryClient(server, &RetryConfig{MaxRetries: 5, InitialDelay: time.Millisecond, MaxDelay: time.Second})
		_, err := stream(client, context.Background())
		var statusError *StatusError
		if !errors.As(err, &statusError) || statusError.ProviderStatus() != status {
			t.Fatalf("status %d: err = %v", status, err)
		}
		if requests.Load() != 1 {
			t.Fatalf("status %d: requests = %d, want 1", status, requests.Load())
		}
	}
}

func TestStreamHonoursRetryAfterAndTheCap(t *testing.T) {
	server, _ := providerServer(t, providerReply{status: 429, headers: map[string]string{"Retry-After-Ms": "40"}}, providerReply{status: 429, headers: map[string]string{"Retry-After": "120"}}, providerReply{})
	client, waits := retryClient(server, &RetryConfig{MaxRetries: 3, InitialDelay: time.Millisecond, MaxDelay: 2 * time.Second})
	if _, err := stream(client, context.Background()); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(*waits) != 2 || (*waits)[0] != 40*time.Millisecond || (*waits)[1] != 2*time.Second {
		t.Fatalf("waits = %v, want [40ms 2s]", *waits)
	}
}

func TestStreamWaitsForTheResetTimeOfARateLimitAnd503KeepsTheBackoff(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(20*time.Second).UnixMilli(), 10)
	server, _ := providerServer(t,
		providerReply{status: 429, headers: map[string]string{"X-RateLimit-Reset": reset}},
		providerReply{status: 503, headers: map[string]string{"X-RateLimit-Reset": reset}},
		providerReply{status: 429, headers: map[string]string{"X-RateLimit-Reset": reset}},
		providerReply{})
	client, waits := retryClient(server, &RetryConfig{MaxRetries: 4, InitialDelay: 500 * time.Millisecond, MaxDelay: time.Minute})
	if _, err := stream(client, context.Background()); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(*waits) != 3 {
		t.Fatalf("waits = %v, want 3", *waits)
	}
	if w := (*waits)[0]; w < 19*time.Second || w > 20*time.Second {
		t.Errorf("a 429 waits until the reset, got %v", w)
	}
	if w := (*waits)[1]; w != time.Second {
		t.Errorf("a 503 keeps the backoff (500ms doubled once), got %v", w)
	}
	if w := (*waits)[2]; w < 19*time.Second || w > 20*time.Second {
		t.Errorf("the next 429 waits until the reset again, got %v", w)
	}
}

func TestStreamDoesNotRetryAFailureAfterEventsArrived(t *testing.T) {
	server, requests := providerServer(t, providerReply{partial: true})
	client, waits := retryClient(server, &RetryConfig{MaxRetries: 5, InitialDelay: time.Millisecond, MaxDelay: time.Second})
	events, err := stream(client, context.Background())
	if err == nil {
		t.Fatal("a stream cut before response.completed must fail")
	}
	if len(events) == 0 {
		t.Fatal("the event that arrived before the cut must have been delivered")
	}
	if requests.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("requests = %d, waits = %v; a started stream is never retried", requests.Load(), *waits)
	}
}

func TestStreamStopDuringAWaitReportsTheStopAndMakesNoFurtherRequest(t *testing.T) {
	server, requests := providerServer(t, providerReply{status: 429})
	client, _ := retryClient(server, &RetryConfig{MaxRetries: 5, InitialDelay: time.Millisecond, MaxDelay: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := stream(client, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

func TestStreamRetriesAConnectionFailure(t *testing.T) {
	server, _ := providerServer(t, providerReply{})
	url := server.URL
	server.Close() // nothing listens any more: every attempt fails to connect
	client := NewClient(Config{APIKey: "k", BaseURL: url + "/v1/", Retry: &RetryConfig{MaxRetries: 2, InitialDelay: time.Millisecond, MaxDelay: time.Second}})
	var waits int
	client.sleep = func(context.Context, time.Duration) error { waits++; return nil }
	client.random = func() float64 { return 0 }
	if _, err := stream(client, context.Background()); err == nil {
		t.Fatal("want a connection error")
	}
	if waits != 2 {
		t.Fatalf("waits = %d, want 2 (one per retry)", waits)
	}
}

func TestStreamWithoutRetryConfigKeepsTheSDKDefaults(t *testing.T) {
	server, requests := providerServer(t, providerReply{status: 429, headers: map[string]string{"Retry-After-Ms": "1"}})
	client := NewClient(Config{APIKey: "test-api-key", BaseURL: server.URL + "/v1/"})
	if _, err := stream(client, context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("provider requests = %d, want 3 (the SDK's own two retries)", got)
	}
}

func TestStreamReportsEachRetryToTheCallback(t *testing.T) {
	server, _ := providerServer(t, providerReply{status: 429}, providerReply{status: 503}, providerReply{})
	var seen []RetryEvent
	client, _ := retryClient(server, &RetryConfig{
		MaxRetries: 3, InitialDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second,
		OnRetry: func(event RetryEvent) { seen = append(seen, event) },
	})
	if _, err := stream(client, context.Background()); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(seen) != 2 || seen[0].Status != 429 || seen[0].Attempt != 1 || seen[1].Status != 503 || seen[1].Attempt != 2 || seen[1].MaxRetries != 3 {
		t.Fatalf("callback events = %+v", seen)
	}
}
