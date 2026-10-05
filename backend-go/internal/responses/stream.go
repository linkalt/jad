// Package responses defines the provider-neutral contract for raw Responses API streams.
package responses

import (
	"context"
	"encoding/json"
)

// Event carries a decoded event discriminator and the original JSON payload.
// Keeping the payload raw preserves provider fields unknown to this Go version.
type Event struct {
	Type string
	Raw  json.RawMessage
}

// EventHandler consumes one stream event. Returning an error stops stream consumption.
type EventHandler func(Event) error

// Streamer sends a Responses API request and forwards events until the stream
// ends or the handler returns an error.
type Streamer interface {
	Stream(context.Context, json.RawMessage, EventHandler) error
}
