package native

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"jad/backend-go/internal/tools"
)

// codeContextBatch runs the items of a read, add or update in order, each as the single
// call it would be with the fields outside items as defaults. Each item is saved on its
// own, so one that fails does not stop or undo the others; the answer says what each did.
// Only when every item fails is the answer a tool error.
func codeContextBatch(arguments map[string]any, call codeContextCall, reader codeContextReader, store codeContextStore, by string) string {
	callID := reader.execution.CallID
	items, _ := arguments["items"].([]any)
	if int64(len(items)) > reader.settings.MaxBatchItems {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "too_many_items", fmt.Sprintf("the call has %d items; max_batch_items is %d", len(items), reader.settings.MaxBatchItems))
	}
	base := make(map[string]any, len(arguments))
	for key, value := range arguments {
		if key != "items" {
			base[key] = value
		}
	}
	var failures []codeContextItemFailure
	var lines, texts []string
	var saved []codeContextOutcome
	budget := reader.settings.MaxOutputBytes
	for index, item := range items {
		number := index + 1
		fields, _ := item.(map[string]any)
		merged := make(map[string]any, len(base)+len(fields))
		for key, value := range base {
			merged[key] = value
		}
		for key, value := range fields {
			merged[key] = value
		}
		itemCall := parseCodeContextCall(merged)
		failure := validateCodeContextCall(itemCall, reader.settings, callID)
		if failure == "" {
			if missing := codeContextMissingFields(itemCall); missing != "" {
				failure = tools.ExecutionError(callID, tools.StatusInvalidInput, "missing_fields", missing)
			}
		}
		if failure == "" && call.action == "read" {
			var text string
			var printed int64
			text, printed, failure = codeContextReadItem(itemCall, reader, store, by, budget)
			if failure == "" {
				budget -= printed
				texts = append(texts, text)
				continue
			}
		}
		if failure == "" {
			outcome := codeContextSave(itemCall, reader, store, by)
			if failure = outcome.failure; failure == "" {
				line := fmt.Sprintf("item %d: #%d %s", number, outcome.entry.ID, map[bool]string{true: "updated", false: "saved"}[call.action == "update"])
				if outcome.note != "" {
					line = fmt.Sprintf("item %d: #%d (%s)", number, outcome.entry.ID, outcome.note)
				}
				lines = append(lines, line)
				saved = append(saved, outcome)
				continue
			}
		}
		parsed := parseCodeContextItemFailure(number, failure)
		failures = append(failures, parsed)
		line := fmt.Sprintf("item %d failed (%s): %s", number, parsed.code, parsed.summary)
		lines = append(lines, line)
		if call.action == "read" {
			texts = append(texts, fmt.Sprintf("code_context task %s %s\n", call.task, line))
		}
	}
	if len(failures) == len(items) {
		information := make([]tools.Information, 0, len(failures))
		for _, failure := range failures {
			information = append(information, tools.Information{Key: fmt.Sprintf("item_%d", failure.number), Value: failure.code + ": " + failure.summary})
		}
		first := failures[0]
		return tools.ExecutionError(callID, first.status, "all_items_failed", fmt.Sprintf("none of the %d items was done; item 1: %s", len(items), first.summary), information...)
	}
	done := len(items) - len(failures)
	if call.action == "read" {
		return fmt.Sprintf("code_context read: %d of %d items done\n\n", done, len(items)) + strings.Join(texts, "\n")
	}
	var output strings.Builder
	fmt.Fprintf(&output, "# code_context %s: %d of %d items done\n", call.action, done, len(items))
	for _, line := range lines {
		output.WriteString("# " + line + "\n")
	}
	document := saved[len(saved)-1].document
	if current, err := store.load(call.task); err == nil {
		document = current
	}
	var entries []codeContextEntry
	statuses := map[int64]codeContextStatus{}
	for _, outcome := range saved {
		if _, listed := statuses[outcome.entry.ID]; listed {
			continue
		}
		entry := outcome.entry
		if index := slices.IndexFunc(document.Entries, func(current codeContextEntry) bool { return current.ID == entry.ID }); index >= 0 {
			entry = document.Entries[index]
		}
		entries = append(entries, entry)
		statuses[entry.ID] = codeContextStatus{}
	}
	output.WriteString(renderCodeContextYAML(document, entries, statuses, nil))
	return output.String()
}

// codeContextMissingFields names what an item still lacks once the defaults are applied,
// as the schema requires of a single call.
func codeContextMissingFields(call codeContextCall) string {
	has := func(keys ...string) bool {
		for _, key := range keys {
			if !call.has[key] {
				return false
			}
		}
		return true
	}
	switch call.action {
	case "read":
		if !has("path", "start", "end") {
			return "a read item needs path, start and end"
		}
	case "add":
		if !has("from_task", "id") && !has("path", "start", "end", "description", "priority") {
			return "an add item needs path, start, end, description and priority, or from_task and id"
		}
	case "update":
		if !has("id") {
			return "an update item needs id"
		}
	}
	return ""
}

// codeContextItemFailure is one item's tool error, read back from its rendered form.
type codeContextItemFailure struct {
	number  int
	status  tools.Status
	code    string
	summary string
}

func parseCodeContextItemFailure(number int, rendered string) codeContextItemFailure {
	failure := codeContextItemFailure{number: number, status: tools.StatusError, code: "failed", summary: oneLine(rendered)}
	header, rest, _ := strings.Cut(rendered, "\n")
	var fields struct {
		Status  tools.Status `json:"status"`
		Summary string       `json:"summary"`
	}
	if json.Unmarshal([]byte(header), &fields) != nil {
		return failure
	}
	failure.status, failure.summary = fields.Status, oneLine(fields.Summary)
	for _, line := range strings.Split(rest, "\n") {
		if code, found := strings.CutPrefix(line, "error_code: "); found {
			failure.code = code
			break
		}
	}
	return failure
}
