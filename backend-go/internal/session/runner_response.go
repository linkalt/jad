package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
