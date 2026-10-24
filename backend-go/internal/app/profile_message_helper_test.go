package app

import (
	"context"
	"errors"
	"time"

	"jad/backend-go/internal/tools/native"
)

// Message sends one message and waits for its end the way a call without an async host
// does: the old synchronous entry point, kept as a test helper over Prepare and Start.
func (m profileMessenger) Message(ctx context.Context, ownerKey, target, message string, queueWait time.Duration, compact bool) (string, error) {
	request := native.ProfileMessageRequest{OwnerKey: ownerKey, Target: target, Message: message, Compact: compact, QueueWait: queueWait}
	if queueWait > 0 {
		request.MaxQueued = 1
	}
	plan, err := m.Prepare(ctx, request)
	if err != nil {
		return "", err
	}
	run, err := plan.Start(ctx)
	if err != nil {
		return "", err
	}
	select {
	case <-run.Done():
	case <-ctx.Done():
		run.Stop("caller_cancelled")
		return "", ctx.Err()
	}
	outcome := run.Outcome()
	if outcome.Err != nil {
		if ctx.Err() != nil && errors.Is(outcome.Err, native.ErrProfileStopped) {
			return "", ctx.Err()
		}
		return "", outcome.Err
	}
	return outcome.Text, nil
}
