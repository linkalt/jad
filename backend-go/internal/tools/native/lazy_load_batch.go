package native

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"jad/backend-go/internal/tools"
)

// lazyRun is one item of a load_tool runs call while it is prepared, run and combined.
type lazyRun struct {
	target    string
	name      string
	arguments map[string]any
	// call is the function call the item stands for, which the catalog judges (state, leaf).
	// It is nil for an item refused before it starts, whose output is the refusal.
	call   json.RawMessage
	output json.RawMessage
}

// lazyRunJudge is what the batch needs of the dispatching catalog beyond dispatching: the
// judgments a round makes of the calls of one response. *tools.Catalog implements it.
type lazyRunJudge interface {
	ChangesState(call json.RawMessage) bool
	IsLeafCall(call json.RawMessage) bool
}

// runLazyTools is load_tool with runs: it runs each listed lazy tool through the dispatching
// catalog, one dispatch per item with the outer call_id and ViaRunner set, so every item takes the
// same path as a single run: the lazy gate, the schema check, plan mode, the sandbox, the target's
// own hooks. The items run under the rules a round applies to the calls of one response: with
// ordered scheduling a batch holding an item that can change state runs one item at a time in the
// order sent, and otherwise up to the call's share of max_parallel_tool_calls run together; a
// leaf item takes one token of the leaf budget. The output holds every item's output in the
// order sent, each under a heading, so an item refused or failing does not hide the others.
// Only a malformed batch or one over lazy_load_max runs nothing.
func runLazyTools(ctx context.Context, set *tools.LazySet, loadMax int, arguments map[string]any, execution tools.ExecutionContext) (json.RawMessage, error) {
	targets, ok := loadToolDelegateRuns(arguments)
	if !ok {
		return textOutput(invalidLoadToolInput(execution, "runs must be a non-empty list of objects with run (a tool name) and optionally arguments (an object), and nothing else"))
	}
	if len(targets) > loadMax {
		return textOutput(tools.ExecutionError(execution.CallID, tools.StatusError, "batch_too_large",
			fmt.Sprintf("runs lists %d tools, the limit is %d (tools.lazy_load_max); nothing was run", len(targets), loadMax),
			tools.Information{Key: "limit", Value: loadMax},
			tools.Information{Key: "requested", Value: len(targets)}))
	}
	runs := make([]lazyRun, len(targets))
	for index, target := range targets {
		runs[index] = prepareLazyRun(set, target, execution)
	}
	executeLazyRuns(ctx, runs, execution)
	return combineLazyRuns(runs)
}

// prepareLazyRun resolves one item. A refusal is kept as the item's output; nothing is dispatched.
func prepareLazyRun(set *tools.LazySet, target tools.DelegateTarget, execution tools.ExecutionContext) lazyRun {
	run := lazyRun{target: target.Target}
	name, refusal := resolveLazyTarget(set, target.Target, execution)
	if refusal != "" {
		run.output = refusalOutput(refusal)
		return run
	}
	call, err := tools.FunctionCall(execution.CallID, name, target.Arguments)
	if err != nil {
		run.output = refusalOutput(tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_input",
			fmt.Sprintf("the arguments of %s cannot be encoded", target.Target)))
		return run
	}
	run.name, run.arguments, run.call = name, target.Arguments, call
	return run
}

// executeLazyRuns dispatches every prepared item and stores each item's output. It returns when all
// have settled. An item that fails, panics or is not started because the run was stopped has an
// error result as its output; nothing here fails the whole call.
func executeLazyRuns(ctx context.Context, runs []lazyRun, execution tools.ExecutionContext) {
	judge, _ := execution.Dispatcher.(lazyRunJudge)
	limit := execution.NestedParallel
	if judge == nil {
		// Without the catalog's judgments nothing says an item is safe beside another.
		limit = 1
	} else if execution.NestedOrdered && limit > 1 {
		for _, run := range runs {
			if run.call != nil && judge.ChangesState(run.call) {
				limit = 1
				break
			}
		}
	}
	limit = max(limit, 1)

	// slots bounds the items in flight; the leaf budget bounds the leaves of every call in the
	// process. This call holds no token while it waits (a batch is no leaf), and an item that
	// holds one waits on nothing that needs another, so a token holder always settles.
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for index := range runs {
		run := &runs[index]
		if run.call == nil {
			continue
		}
		slots <- struct{}{}
		if err := ctx.Err(); err != nil {
			<-slots
			run.output = refusalOutput(tools.ExecutionError(execution.CallID, tools.StatusError, "not_run",
				"This tool was not run because the run was stopped first.", tools.Information{Key: "tool", Value: run.target}))
			continue
		}
		leaf := judge != nil && judge.IsLeafCall(run.call)
		if leaf {
			execution.LeafBudget.Acquire()
		}
		wg.Add(1)
		go func(run *lazyRun, leaf bool) {
			defer wg.Done()
			defer func() {
				if leaf {
					execution.LeafBudget.Release()
				}
				<-slots
			}()
			defer func() {
				if recovered := recover(); recovered != nil {
					slog.Error("lazy tool run panicked", "tool", run.name, "call_id", execution.CallID, "panic", recovered)
					run.output = failedRunOutput(execution, run)
				}
			}()
			nested := execution
			nested.ViaRunner = true
			output, err := tools.DispatchCall(ctx, nested.Dispatcher, run.name, run.arguments, nested)
			if err != nil {
				slog.Error("lazy tool run failed", "tool", run.name, "call_id", execution.CallID, "error", err)
				run.output = failedRunOutput(execution, run)
				return
			}
			run.output = output
		}(run, leaf)
	}
	wg.Wait()
}

func failedRunOutput(execution tools.ExecutionContext, run *lazyRun) json.RawMessage {
	return refusalOutput(tools.ExecutionError(execution.CallID, tools.StatusError, "tool_execution_failed", "Tool "+run.name+" failed during execution"))
}

// refusalOutput is a text result as an output value.
func refusalOutput(text string) json.RawMessage {
	encoded, err := tools.MarshalJSON(text)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return encoded
}

// combineLazyRuns is the output of the call: every item's output in the order sent, each under
// the heading "--- run N of M: <tool> ---". When every output is text the result is one text;
// when any is a content array (images, files) the result is one content array in which a heading
// text part precedes each item's parts, the parts passed through byte for byte.
func combineLazyRuns(runs []lazyRun) (json.RawMessage, error) {
	texts := make([]string, len(runs))
	allText := true
	for index, run := range runs {
		if err := json.Unmarshal(run.output, &texts[index]); err != nil {
			allText = false
			break
		}
	}
	headings := make([]string, len(runs))
	for index, run := range runs {
		headings[index] = "--- run " + strconv.Itoa(index+1) + " of " + strconv.Itoa(len(runs)) + ": " + run.target + " ---"
	}
	if allText {
		sections := make([]string, len(runs))
		for index := range runs {
			sections[index] = headings[index] + "\n" + texts[index]
		}
		return tools.MarshalJSON(strings.Join(sections, "\n\n"))
	}
	parts := make([]json.RawMessage, 0, 2*len(runs))
	for index, run := range runs {
		heading, err := tools.MarshalJSON(textPart{Type: "input_text", Text: headings[index]})
		if err != nil {
			return nil, err
		}
		parts = append(parts, heading)
		var itemParts []json.RawMessage
		switch {
		case json.Unmarshal(run.output, &itemParts) == nil && itemParts != nil:
			parts = append(parts, itemParts...)
		default:
			var text string
			if json.Unmarshal(run.output, &text) != nil {
				text = string(run.output)
			}
			part, err := tools.MarshalJSON(textPart{Type: "input_text", Text: text})
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
	}
	var combined strings.Builder
	combined.WriteByte('[')
	for index, part := range parts {
		if index > 0 {
			combined.WriteByte(',')
		}
		combined.Write(part)
	}
	combined.WriteByte(']')
	return json.RawMessage(combined.String()), nil
}

// textPart is a text part of a content array output.
type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
