package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"example.com/agent-harness/backend-go/internal/imagestore"
	"example.com/agent-harness/backend-go/internal/responses"
	"example.com/agent-harness/backend-go/internal/transcript"
)

// runResponse streams and commits one provider response. Each tool round gets
// its own response turn while remaining part of the same session turn.
func (r *Runner) runResponse(ctx context.Context, request json.RawMessage, sessionTurn, sessionStep int64, turnID string, options RunOptions) (result RunResult, runErr error) {
	result.TurnID = turnID
	var turn *transcript.ResponseTurn
	turnClosed := false
	defer func() {
		if turn == nil || turnClosed {
			return
		}
		if closeErr := turn.Close(); closeErr != nil {
			closeErr = fmt.Errorf("close incomplete response turn: %w", closeErr)
			if runErr == nil {
				runErr = closeErr
			} else {
				runErr = errors.Join(runErr, closeErr)
			}
		}
	}()

	ensureTurn := func() error {
		if turn != nil {
			return nil
		}
		var beginErr error
		turn, beginErr = r.store.BeginTurn(turnID)
		if beginErr != nil {
			return fmt.Errorf("begin response turn: %w", beginErr)
		}
		if options.OnTurnStarted != nil {
			options.OnTurnStarted(turn.PartialContentPath())
		}
		return nil
	}

	stagedItems := make(map[int64]json.RawMessage)
	nextOutputIndex := int64(0)
	terminalCommitted := false
	streamErr := r.streamer.Stream(ctx, request, func(event responses.Event) error {
		if err := ensureTurn(); err != nil {
			return err
		}
		if !json.Valid(event.Raw) {
			return errors.New("Responses stream event must contain valid JSON")
		}
		eventFields, err := decodeJSONObject(event.Raw)
		if err != nil {
			return fmt.Errorf("decode Responses stream event: %w", err)
		}
		typeRaw, exists := rawField(eventFields, "type")
		if !exists {
			return errors.New("Responses stream event must contain a type")
		}
		var eventType string
		if err := json.Unmarshal(typeRaw, &eventType); err != nil || eventType == "" {
			return errors.New("Responses stream event type must be a non-empty string")
		}
		// A generated image goes to the image store before anything else sees the event,
		// so the stream, the staged item and the transcript carry a reference, not base64.
		if options.ImageStore != nil {
			if rewritten, changed := externalizeEventImages(event.Raw, eventType, eventFields, options.ImageStore); changed {
				event.Raw = rewritten
				if eventFields, err = decodeJSONObject(event.Raw); err != nil {
					return fmt.Errorf("decode Responses stream event: %w", err)
				}
			}
		}
		if options.OnEvent != nil {
			if err := options.OnEvent(event); err != nil {
				return err
			}
		}

		switch eventType {
		case "response.output_item.done":
			itemRaw, exists := rawField(eventFields, "item")
			if !exists {
				return errors.New("response.output_item.done event has no item")
			}
			normalizedFields := []rawObjectField{
				{key: "type", value: json.RawMessage(`"response.output_item.done"`)},
				{key: "item", value: cloneRaw(itemRaw)},
			}
			outputIndex := nextOutputIndex
			if indexRaw, exists := rawField(eventFields, "output_index"); exists && isJSONNumber(indexRaw) {
				outputIndex, err = parseSafeOutputIndex(indexRaw)
				if err != nil {
					return err
				}
				normalizedFields = append(normalizedFields, rawObjectField{key: "output_index", value: cloneRaw(indexRaw)})
			}
			normalizedEvent, err := encodeJSONObject(normalizedFields)
			if err != nil {
				return fmt.Errorf("normalize output-item event: %w", err)
			}
			if err := turn.StageOutputItem(normalizedEvent); err != nil {
				return err
			}
			stagedItems[outputIndex] = cloneRaw(itemRaw)
			if outputIndex >= nextOutputIndex {
				nextOutputIndex = outputIndex + 1
			}
			return nil
		case "response.completed", "response.incomplete":
			completedEvent, err := makeCompletedEvent(event.Raw, sessionTurn, sessionStep, stagedItems)
			if err != nil {
				return err
			}
			commit, err := turn.CommitWithObserver(completedEvent, func(commit transcript.CommitResult) {
				if options.OnResponseCommitted != nil {
					options.OnResponseCommitted(cloneRaw(completedEvent), commit)
				}
			})
			if err != nil {
				return fmt.Errorf("commit completed Responses event: %w", err)
			}
			turnClosed = true
			terminalCommitted = true
			result.Event = cloneRaw(completedEvent)
			result.Commit = commit
			return errTerminalResponseCommitted
		case "response.failed", "error":
			return terminalFailureError(event.Raw, eventType)
		default:
			return nil
		}
	})
	if streamErr != nil && !errors.Is(streamErr, errTerminalResponseCommitted) {
		return result, streamErr
	}
	if !terminalCommitted {
		if streamErr != nil {
			return result, streamErr
		}
		return result, errors.New("Responses stream ended before response.completed")
	}
	return result, nil
}

// externalizeEventImages moves the generated images of an output_item.done or a
// completed event into the image store and returns the event with references in their
// place. It reports false, with the event untouched, when nothing was stored.
func externalizeEventImages(raw json.RawMessage, eventType string, fields []rawObjectField, images *imagestore.Store) (json.RawMessage, bool) {
	var key string
	switch eventType {
	case "response.output_item.done":
		key = "item"
	case "response.completed", "response.incomplete":
		key = "response"
	default:
		return raw, false
	}
	value, exists := rawField(fields, key)
	if !exists {
		return raw, false
	}
	var replaced json.RawMessage
	if key == "item" {
		item, stats := images.ExternalizeItem(value)
		if stats.Stored == 0 {
			return raw, false
		}
		replaced = item
	} else {
		response, err := decodeJSONObject(value)
		if err != nil {
			return raw, false
		}
		output, hasOutput := rawField(response, "output")
		if !hasOutput {
			return raw, false
		}
		shrunk, stats := images.ExternalizeOutput(output)
		if stats.Stored == 0 {
			return raw, false
		}
		for index := range response {
			if response[index].key == "output" {
				response[index].value = shrunk
			}
		}
		encoded, err := encodeJSONObject(response)
		if err != nil {
			return raw, false
		}
		replaced = encoded
	}
	updated := append([]rawObjectField(nil), fields...)
	for index := range updated {
		if updated[index].key == key {
			updated[index].value = replaced
		}
	}
	encoded, err := encodeJSONObject(updated)
	if err != nil {
		return raw, false
	}
	return encoded, true
}
