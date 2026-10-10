package openaiapi

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
)

// RetryConfig controls how a failed request that opens a response stream is retried.
// It mirrors the TypeScript backend's provider-retry module, which has the same rules.
// Only the opening request is retried: once events have arrived, partial output was
// already shown and staged, so a failure after that point is never retried.
type RetryConfig struct {
	// MaxRetries is the number of retries after the first attempt; 0 turns retrying off.
	MaxRetries int
	// InitialDelay is the wait before the first retry. Each later wait doubles.
	InitialDelay time.Duration
	// MaxDelay is the longest wait between attempts, and the cap for a Retry-After.
	MaxDelay time.Duration
	// OnRetry, when set, is called before each wait; the app uses it to log the retry.
	OnRetry func(RetryEvent)
}

// RetryEvent describes one retry that is about to wait.
type RetryEvent struct {
	Attempt    int
	MaxRetries int
	Delay      time.Duration
	// Status is the provider status that caused the retry, or 0 for a transport failure.
	Status int
}

func statusOf(err error) int {
	var apiError *openai.Error
	if errors.As(err, &apiError) {
		return apiError.StatusCode
	}
	return 0
}

// isRetryableError reports whether a failed opening request is worth another attempt:
// a status the SDK also retries (408, 409, 429, 5xx), whatever the provider's
// x-should-retry header says, and a transport failure such as a reset connection.
// The caller has already ruled out a stopped run.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var apiError *openai.Error
	if errors.As(err, &apiError) && apiError.StatusCode > 0 {
		if apiError.Response != nil {
			switch apiError.Response.Header.Get("x-should-retry") {
			case "true":
				return true
			case "false":
				return false
			}
		}
		code := apiError.StatusCode
		return code == http.StatusRequestTimeout || code == http.StatusConflict ||
			code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// An error that says it is permanent (an unusable credential file) fails the same
	// way every time, even though the transport wraps it in a retryable *url.Error.
	var permanent interface{ Permanent() bool }
	if errors.As(err, &permanent) && permanent.Permanent() {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	// A URL that cannot be parsed fails the same way every time.
	var urlError *url.Error
	if errors.As(err, &urlError) && urlError.Op == "parse" {
		return false
	}
	var netError net.Error
	return errors.As(err, &netError)
}

// retryAfter reads the wait the provider asked for: Retry-After-Ms, else Retry-After as
// seconds or an HTTP date. A negative or unreadable value counts as absent.
func retryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	if value := strings.TrimSpace(header.Get("Retry-After-Ms")); value != "" {
		if millis, err := strconv.ParseFloat(value, 64); err == nil && !math.IsNaN(millis) && !math.IsInf(millis, 0) && millis >= 0 {
			return time.Duration(millis * float64(time.Millisecond)), true
		}
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now)), true
	}
	return 0, false
}

const (
	// epochMillisFloor is the smallest value read as an epoch in milliseconds (1e12 ms is
	// September 2001).
	epochMillisFloor = 1e12
	// epochSecondsFloor is the smallest value read as an epoch in seconds (1e9 s is
	// September 2001).
	epochSecondsFloor = 1e9
)

// rateLimitReset reads the wait until the time named by x-ratelimit-reset on a 429.
// OpenRouter sends the header, as an epoch in milliseconds, on a platform rate limit and
// no Retry-After. Providers differ in the unit, so only a value that is plainly an epoch
// is used: at least 1e12 is milliseconds, at least 1e9 is seconds. A small number (a
// delta), a duration string, an unreadable value and a time that has already passed are
// absent, and any other status is ignored, so a format that was not verified changes
// nothing. It follows rateLimitResetMs of the removed TypeScript backend (tag legacy-final).
func rateLimitReset(header http.Header, status int, now time.Time) (time.Duration, bool) {
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	raw := strings.TrimSpace(header.Get("X-Ratelimit-Reset"))
	if raw == "" || !isPlainDecimal(raw) {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	var resetAtMillis float64
	switch {
	case value >= epochMillisFloor:
		resetAtMillis = value
	case value >= epochSecondsFloor:
		resetAtMillis = value * 1000
	default:
		return 0, false
	}
	wait := resetAtMillis - float64(now.UnixMilli())
	if wait <= 0 || math.IsInf(wait, 0) {
		return 0, false
	}
	return time.Duration(wait * float64(time.Millisecond)), true
}

// isPlainDecimal reports whether s is digits with at most one fractional part, the same
// shape the TypeScript backend accepts (no sign, exponent, hex or text).
func isPlainDecimal(s string) bool {
	digits, dot := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot && digits > 0 && i < len(s)-1:
			dot = true
		default:
			return false
		}
	}
	return digits > 0
}

// retryDelay is the wait before retry number attempt (0 for the first retry). A Retry-After
// the provider sent replaces the backoff, and on a 429 without one the provider's
// x-ratelimit-reset does; either way the wait never exceeds MaxDelay. The backoff doubles
// from InitialDelay up to MaxDelay, minus up to a quarter as jitter. random returns [0, 1).
// status is the provider status that caused the retry, or 0 for a transport failure.
func retryDelay(config RetryConfig, attempt, status int, header http.Header, now time.Time, random func() float64) time.Duration {
	if requested, ok := retryAfter(header, now); ok {
		return min(requested, config.MaxDelay)
	}
	if requested, ok := rateLimitReset(header, status, now); ok {
		return min(requested, config.MaxDelay)
	}
	backoff := float64(config.InitialDelay) * math.Pow(2, float64(attempt))
	if math.IsInf(backoff, 0) || backoff >= float64(config.MaxDelay) {
		backoff = float64(config.MaxDelay)
	}
	return time.Duration(math.Floor(backoff * (1 - random()*0.25)))
}

// responseHeader returns the headers of the failed response, or none for a transport failure.
func responseHeader(err error) http.Header {
	var apiError *openai.Error
	if errors.As(err, &apiError) && apiError.Response != nil {
		return apiError.Response.Header
	}
	return nil
}

// sleepContext waits for d, or returns the context's error as soon as the run is stopped.
func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func defaultRandom() float64 { return rand.Float64() }
