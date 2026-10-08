package openaiauth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RefreshOptions control one refresh.
type RefreshOptions struct {
	// IfOlderThan skips the request when the stored tokens were refreshed more recently than
	// this. Zero always refreshes.
	IfOlderThan time.Duration
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// RefreshResult says what Refresh did.
type RefreshResult struct {
	// Refreshed is false when IfOlderThan found the tokens recent enough.
	Refreshed bool
	// Age is the time since the stored tokens were refreshed, when it could be read.
	Age time.Duration
	// AgeKnown is false when the file has no readable last_refresh.
	AgeKnown bool
}

// ErrNeedLogin means the stored refresh token is missing or was rejected by the issuer.
var ErrNeedLogin = errors.New("sign in again with the login command")

// Refresh trades the stored refresh token for new tokens and rewrites the credential file. The
// whole exchange runs under the file lock: a refresh token is usually rotated by the issuer, so
// a second concurrent refresh with the same token would fail and could invalidate the login.
// A failed request leaves the file untouched.
func Refresh(ctx context.Context, store *Store, client *Client, options RefreshOptions) (*RefreshResult, error) {
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	result := &RefreshResult{}
	err := store.Update(ctx, func(current *File) (*File, error) {
		if current == nil {
			return nil, fmt.Errorf("%s does not exist: %w", store.Path, ErrNeedLogin)
		}
		if current.RefreshToken == "" {
			return nil, fmt.Errorf("the credential file has no refresh token: %w", ErrNeedLogin)
		}
		if stored, err := time.Parse(time.RFC3339, current.LastRefresh); err == nil {
			result.Age, result.AgeKnown = now().Sub(stored), true
		}
		if options.IfOlderThan > 0 && result.AgeKnown && result.Age < options.IfOlderThan {
			return nil, errSkip
		}
		tokens, err := client.Refresh(ctx, current.RefreshToken)
		if err != nil {
			var tokenErr *TokenError
			if errors.As(err, &tokenErr) && tokenErr.Rejected() {
				return nil, fmt.Errorf("%w: %w", err, ErrNeedLogin)
			}
			return nil, err
		}
		result.Refreshed = true
		return current.Apply(tokens, now())
	})
	if errors.Is(err, errSkip) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// errSkip aborts Update without writing when a refresh is not due.
var errSkip = errors.New("refresh not due")
