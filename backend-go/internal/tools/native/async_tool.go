package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

const (
	asyncToolID = "native.async"

	asyncActionWait   = "wait"
	asyncActionList   = "list"
	asyncActionStatus = "status"
	asyncActionStop   = "stop"

	asyncStopReason = "stopped_by_agent"
)

var asyncInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["wait","list","status","stop"]},
    "wait_ms":{"type":"integer","minimum":0,"description":"wait: most time to block; 0 polls."},
    "ids":{"type":"array","items":{"type":"string"},"maxItems":32,"description":"wait: only these handles. Default: all yours."},
    "id":{"type":"string","description":"status, stop: handle id."},
    "state":{"type":"string","enum":["active","done","failed","stopped","timed_out","lost","all"],"description":"list: default active."},
    "kind":{"type":"string","description":"list: process, subagent, ..."},
    "limit":{"type":"integer","minimum":1,"description":"list: page size."},
    "cursor":{"type":"string","description":"list: from the previous page."}
  },
  "required":[],
  "additionalProperties":false
}`)

// asyncActionFields are the fields each action accepts besides action itself.
var asyncActionFields = map[string][]string{
	asyncActionWait:   {"wait_ms", "ids"},
	asyncActionList:   {"state", "kind", "limit", "cursor"},
	asyncActionStatus: {"id"},
	asyncActionStop:   {"id"},
}

var asyncFieldNames = []string{"wait_ms", "ids", "id", "state", "kind", "limit", "cursor"}

// NewAsyncTool is the one tool that waits for, lists, inspects and stops the handles that
// the start tools (process, ...) return. It acts only on the calling session's handles.
func NewAsyncTool(host *AsyncHost) (tools.Tool, error) {
	validator, err := tools.CompileSchema(asyncInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native async schema: %w", err)
	}
	if host == nil {
		return tools.Tool{}, errors.New("native async tool needs an async host")
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          asyncToolID,
			Source:      "native",
			Name:        "async",
			Description: asyncDescription,
			InputSchema: append(json.RawMessage(nil), asyncInputSchema...),
		},
		Validator:        validator,
		ExampleOperation: json.RawMessage(`{"action":"wait","wait_ms":10000}`),
		// wait, list and status change nothing and may run beside other read-only calls;
		// a stop is serialized (see MayChangeFiles).
		ReadOnly:           true,
		MayChangeFiles:     func(arguments map[string]any) bool { return asyncActionFor(arguments) == asyncActionStop },
		NormalizeArguments: normalizeAsyncArguments,
		RejectedProperties: map[string]string{asyncBlockAlias: "async takes wait_ms; block_ms is the start tools' name for it. Send wait_ms only."},
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeAsync(ctx, host, arguments, execution)
		},
	}, nil
}

const asyncDescription = "Manage background work started by other tools. wait: block until at least one handle finishes and return every finished one. list: your handles, active by default. status: one handle. stop: end one handle. A start that returned a handle id is still running; collect it with wait in a later call."

// asyncBlockAlias is the start tools' block_ms. A wait that sends it means wait_ms, so it is
// taken as wait_ms; sent beside wait_ms it is refused with a hint rather than one value chosen.
const asyncBlockAlias = "block_ms"

func normalizeAsyncArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	if value, ok := normalized[asyncBlockAlias]; ok {
		if _, both := normalized["wait_ms"]; !both {
			delete(normalized, asyncBlockAlias)
			normalized["wait_ms"] = value
		}
	}
	for _, key := range []string{"action", "id", "state", "kind", "cursor"} {
		if value, ok := normalized[key].(string); ok {
			normalized[key] = strings.TrimSpace(value)
		}
	}
	if action, ok := normalized["action"].(string); ok {
		action = strings.ToLower(action)
		switch action {
		case "kill", "cancel", "terminate":
			action = asyncActionStop
		case "info", "get":
			action = asyncActionStatus
		}
		normalized["action"] = action
	}
	if state, ok := normalized["state"].(string); ok {
		normalized["state"] = strings.ToLower(state)
	}
	for _, key := range []string{"wait_ms", "limit"} {
		if number, ok := safeInteger(normalized[key]); ok {
			minimum := int64(0)
			if key == "limit" {
				minimum = 1
			}
			normalized[key] = json.Number(fmt.Sprint(max(number, minimum)))
		}
	}
	return normalized
}

// asyncActionFor is the action of a call: explicit, else inferred. Nothing at all is a
// list, never a wait, so a bare call cannot block.
func asyncActionFor(arguments map[string]any) string {
	if action, _ := arguments["action"].(string); action != "" {
		return action
	}
	_, hasWait := arguments["wait_ms"]
	_, hasIDs := arguments["ids"]
	_, hasID := arguments["id"]
	switch {
	case hasWait || hasIDs:
		return asyncActionWait
	case hasID:
		return asyncActionStatus
	default:
		return asyncActionList
	}
}

func executeAsync(ctx context.Context, host *AsyncHost, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
	callID := execution.CallID
	action := asyncActionFor(arguments)
	allowed, known := asyncActionFields[action]
	if !known {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_action", "action must be wait, list, status or stop"), nil
	}
	for _, field := range asyncFieldNames {
		if _, present := arguments[field]; present && !containsName(allowed, field) {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_field",
				fmt.Sprintf("%s does not accept %s", action, field), tools.Information{Key: "field", Value: field}), nil
		}
	}
	manager := host.Manager()
	cfg := host.Config()
	// A sub-agent's async tool sees only the handles its own run owns.
	owner := execution.SessionKey
	if execution.ProcessOwner != "" {
		owner = execution.ProcessOwner
	}
	switch action {
	case asyncActionWait:
		var ids []string
		if raw, ok := arguments["ids"].([]any); ok {
			for _, item := range raw {
				if id, ok := item.(string); ok && strings.TrimSpace(id) != "" {
					ids = append(ids, strings.TrimSpace(id))
				}
			}
		}
		var waitMs *int64
		if value, ok := safeInteger(arguments["wait_ms"]); ok {
			waitMs = &value
		}
		endWaits := host.beginProfileMessageWaits(owner, ids)
		result, err := manager.Wait(ctx, owner, ids, cfg.WaitDuration(waitMs))
		endWaits()
		switch {
		case errors.Is(err, asyncmgr.ErrUnknownHandle):
			return asyncUnknownHandle(callID, err), nil
		case err != nil && (ctx.Err() != nil):
			return "", ctx.Err()
		case err != nil:
			return tools.ExecutionError(callID, tools.StatusError, "async_wait_failed", err.Error()), nil
		}
		information := []tools.Information{
			{Key: "action", Value: action},
			{Key: "signals", Value: nonNilEntries(result.Signals)},
			{Key: "pending", Value: result.Pending},
		}
		if result.More {
			information = append(information, tools.Information{Key: "more", Value: true})
		}
		if result.Dropped > 0 {
			information = append(information, tools.Information{Key: "dropped_unread", Value: result.Dropped})
		}
		information = appendLatest(information, host.latestBlocks(owner, host.liveHandleIDs(owner, ids)))
		return renderAsync(callID, fmt.Sprintf("%d signal(s), %d pending", len(result.Signals), result.Pending), information)
	case asyncActionList:
		limit := int64(0)
		if value, ok := safeInteger(arguments["limit"]); ok {
			limit = value
		}
		query := asyncmgr.ListQuery{
			Owner: owner, State: asyncText(arguments["state"]), Kind: asyncmgr.Kind(asyncText(arguments["kind"])),
			Limit: limit, Cursor: asyncText(arguments["cursor"]),
		}
		result, err := manager.List(query)
		if err != nil {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_list", err.Error()), nil
		}
		items := result.Items
		if items == nil {
			items = []asyncmgr.ListItem{}
		}
		information := []tools.Information{
			{Key: "action", Value: action},
			{Key: "counts", Value: result.Counts},
			{Key: "items", Value: items},
		}
		if result.Next != "" {
			information = append(information, tools.Information{Key: "next", Value: result.Next})
		}
		if result.Dropped > 0 {
			information = append(information, tools.Information{Key: "dropped_unread", Value: result.Dropped})
		}
		var live []string
		for _, item := range items {
			if !item.State.Terminal() {
				live = append(live, item.ID)
			}
		}
		information = appendLatest(information, host.latestBlocks(owner, live))
		return renderAsync(callID, fmt.Sprintf("%d handle(s) listed, %d active", len(items), result.Counts.Active), information)
	}
	id := asyncText(arguments["id"])
	if id == "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_target", action+" requires id"), nil
	}
	var entry asyncmgr.Entry
	var err error
	if action == asyncActionStatus {
		entry, err = manager.Status(owner, id)
	} else {
		if current, statusErr := manager.Status(owner, id); statusErr == nil && current.State == asyncmgr.StateLost {
			return tools.ExecutionError(callID, tools.StatusBlocked, "handle_lost", "The work belongs to a previous backend instance and cannot be safely signaled", tools.Information{Key: "id", Value: id}), nil
		}
		entry, err = manager.Stop(ctx, owner, id, asyncStopReason)
	}
	if errors.Is(err, asyncmgr.ErrUnknownHandle) {
		return asyncUnknownHandle(callID, err), nil
	}
	if errors.Is(err, asyncmgr.ErrForeignHandle) {
		return tools.ExecutionError(callID, tools.StatusBlocked, "handle_foreign", "The work runs in another backend server that shares this tool storage; only that server can stop it", tools.Information{Key: "id", Value: id}), nil
	}
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "async_"+action+"_failed", err.Error()), nil
	}
	information := []tools.Information{{Key: "action", Value: action}, {Key: "entry", Value: entry}}
	if action == asyncActionStatus {
		information = appendLatest(information, host.latestBlocks(owner, []string{id}))
	}
	return renderAsync(callID, fmt.Sprintf("%s %s is %s", entry.Kind, entry.ID, entry.State), information)
}

// appendLatest adds one "latest" line per block: id, kind, then the block text.
func appendLatest(information []tools.Information, blocks []latestBlock) []tools.Information {
	if len(blocks) > 0 {
		information = append(information, tools.Information{Key: "latest", Value: blocks})
	}
	return information
}

func asyncText(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func nonNilEntries(entries []asyncmgr.Entry) []asyncmgr.Entry {
	if entries == nil {
		return []asyncmgr.Entry{}
	}
	return entries
}

func asyncUnknownHandle(callID string, err error) string {
	return tools.ExecutionError(callID, tools.StatusNotFound, "unknown_handle", "No handle of yours has that id", tools.Information{Key: "detail", Value: err.Error()})
}

func renderAsync(callID, summary string, information []tools.Information) (string, error) {
	result, err := tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: summary, Information: information})
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "tool_output_failed", "The async result could not be rendered"), nil
	}
	return result, nil
}
