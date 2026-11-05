// Package session contains provider-neutral session orchestration and optional
// native-tool round handling. HTTP routes, MCP, and OAuth remain outside.
package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/imageresize"
	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/memorytrace"
	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tracing"
	"jad/backend-go/internal/transcript"
)

const maxSafeJSONInteger int64 = 9007199254740991

var errTerminalResponseCommitted = errors.New("terminal response committed")

// ModelSettings is the subset of loaded model configuration needed to build a
// Responses API request. Request preserves arbitrary model.request fields.
type ModelSettings struct {
	Model   string
	Request json.RawMessage
	// HostedTools are provider-side tool definitions sent after the function tools.
	// They are passed on as given: the provider runs them, so there is no tool code.
	HostedTools []json.RawMessage
	PromptRole  string
	PromptText  string
	// PromptCacheKey, when set, is sent as the request's prompt_cache_key.
	PromptCacheKey string
	// TextFormat, when set, is sent as the request's text.format, beside any other text
	// fields of Request; it replaces a text.format Request holds.
	TextFormat json.RawMessage
}

type inputTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type userInputMessage struct {
	Type    string             `json:"type"`
	Role    string             `json:"role"`
	Content []inputTextContent `json:"content"`
}

type promptMessage struct {
	Role    string             `json:"role"`
	Content []inputTextContent `json:"content"`
}

// StateContextPrompt is one derived-state block supplied to a request without
// rewriting transcript history. It is sent after the first After history items.
type StateContextPrompt struct {
	Role  string
	Text  string
	After int
}

// RunOptions provides per-run identifiers and an optional raw-event observer.
type AutomaticCompactionSettings struct {
	Enabled        bool
	TriggerTokens  int64
	TurnType       string
	KeepTurns      int
	KeepToolPairs  int
	BackupMode     string
	BackupMinTurns int
	// Summary, when not nil, has the model summarize the turns a compaction drops; see
	// CompactionSummarySettings.
	Summary *CompactionSummarySettings
	// OnSummaryError, when not nil, is told about a summary request that failed.
	OnSummaryError func(error)
	// Overflow, when not nil and Enabled is true, compacts and sends a round again when
	// the provider refused it for being over the context window; see OverflowSettings.
	Overflow *OverflowSettings
}

// OverflowSettings are the normalized compaction.overflow settings.
type OverflowSettings struct {
	// MaxAttempts is how many overflow compactions one run may make.
	MaxAttempts int
	// Patterns are matched, case-insensitively, against the provider error's text.
	Patterns []string
	// OnCompacted, when not nil, is told about each overflow compaction: the attempt
	// number (from 1), whether it rewrote the transcript, and the provider's error.
	OnCompacted func(attempt int, compacted bool, providerErr error)
}

// HookReport is the report of one run_start, run_end or pre_compact command, for a caller
// that shows it live. It is never stored.
type HookReport struct {
	Event   string
	Trigger string
	Report  string
}

type RunOptions struct {
	TurnID string
	// OnHookReport receives each run hook report as it happens; nil drops them (they are
	// always written to the server log).
	OnHookReport  func(HookReport)
	OnEvent       responses.EventHandler
	OnTurnStarted func(partialContentPath string)
	// OnInputAccepted runs once the user input is stored, before anything that
	// can still fail, so a caller knows which turn and message a later failure
	// belongs to. A caller that calls PersistInput itself fires it too.
	OnInputAccepted     func(PersistedInput)
	OnResponseCommitted func(event json.RawMessage, commit transcript.CommitResult)
	OnToolResult        func(item json.RawMessage)
	MemoryTrace         *memorytrace.Trace
	// ImageResizer shrinks the images of the history copy that is sent to the provider.
	// Nil sends the history as stored. The stored history is never changed.
	ImageResizer *imageresize.Resizer
	// ImageStore keeps large tool-result images in files: they are written there before the
	// result is persisted, and read back into the request that goes to the provider. Nil
	// keeps every image inline in the transcript.
	ImageStore    *imagestore.Store
	ToolCatalog   *tools.Catalog
	ToolExecution tools.ExecutionContext
	// Checkpoint records the workspace before the run first changes it and when
	// the run ends. Nil, or a service whose setting is off, means no checkpoint.
	// A snapshot that fails is a recorded warning and never stops the call it precedes.
	Checkpoint          *checkpoint.Service
	StateContext        []StateContextPrompt
	RefreshStateContext func(context.Context) ([]StateContextPrompt, error)
	AutomaticCompaction *AutomaticCompactionSettings
	// CompactHistory replaces the store's compaction when the caller must keep
	// derived data (such as cumulative usage) consistent across the rewrite.
	CompactHistory func(transcript.CompactionOptions) (transcript.CompactionResult, error)
	// NextSessionTurn replaces the transcript-derived turn number. Compaction
	// rewrites the transcript, so the cumulative usage count is the only source
	// that keeps numbering continuous across it.
	NextSessionTurn func() (int64, error)
	// OnUsageChanged runs after an automatic compaction rewrote the transcript.
	OnUsageChanged func()
	MaxToolRounds  int
	// MaxRunCostUSD is the most one run may cost, by the CostMeter of its context, before
	// it starts another round; zero means no cap. Run gives every run a meter of its own.
	MaxRunCostUSD float64
	// Budget, when set, counts what every response of the run used and refuses the run,
	// or its next round, once a session or daily cap is reached.
	Budget *Budget
	// MaxRunTokens is the most tokens one run may use before it starts another
	// provider round; zero means no cap. Counted from the usage the provider reported.
	MaxRunTokens int64
	// MaxParallelToolCalls is the most calls of one provider response that run
	// at the same time; a value below one runs them one after another.
	MaxParallelToolCalls int
	// ToolCallScheduling is "ordered" (the default of the session setting) or
	// "concurrent"; see toolBatchConcurrency. An empty value runs concurrently, so
	// a caller that builds RunOptions by hand keeps the old behaviour.
	ToolCallScheduling string
	// LeafBudget is the process-wide budget of leaf tool calls shared by every run; nil
	// runs the leaf calls of a response without one, as a caller that builds RunOptions by hand gets.
	LeafBudget *tools.LeafBudget
	// PersistedInput is set when PersistInput already stored the user input, so
	// Run must not normalize, repair or append it again. A caller does this when
	// work that can fail (reading the model file, connecting tool servers,
	// resolving credentials) has to happen after the input is durable.
	PersistedInput *PersistedInput
	// OutputFormat, when set, is the Responses text.format every request of the run sends,
	// for example {"type":"json_schema","name":...,"schema":...}: the provider then
	// constrains the final answer to it. It never reaches the transcript.
	OutputFormat json.RawMessage
	// Plan marks a run started in plan mode. The caller that prepares the tools narrows
	// ToolCatalog for it and sets PlanPrompt; Run itself only sends PlanPrompt.
	Plan bool
	// Tools, when not nil, narrows ToolCatalog to the tools these tools.allow entries
	// cover; the caller that prepares the tools applies it.
	Tools []string
	// ExecutePlan puts plan_mode.execute_prompt before the input; the caller that stores
	// the input applies it.
	ExecutePlan bool
	// PlanPrompt, when its Text is set, is sent after the history in every request of the
	// run, with its Role; its After is ignored. It never reaches the transcript.
	PlanPrompt StateContextPrompt
	// Steer holds the user messages sent while the run works. They are stored as user
	// input before each provider round, and a message that waits when the run would end
	// makes it take one more round. The run closes it when it returns. Nil takes none.
	Steer *SteerQueue
	// OnSteer, when not nil, is told about the items of each steering message once stored.
	OnSteer func(items [][]byte)
	// OnSteerDropped, when not nil, is told about the steering messages that were accepted
	// but never stored because the run ended first (it failed or was stopped).
	OnSteerDropped func(inputs []json.RawMessage)
	// OnStop, when set, is asked each time the run would end without an error and with no
	// steering message waiting. A non-empty message makes the run store it as one message
	// of StopMessageRole and go another round, at most StopMaxContinues times per run.
	// continues is how many times it already did in this run.
	OnStop           func(ctx context.Context, turnID, answer string, continues int) (string, error)
	StopMaxContinues int
	StopMessageRole  string
	// Notes are sent after the history (and before PlanPrompt) in every request of the run,
	// each with its Role; their After is ignored. They never reach the transcript. The
	// MCP resource changes since the profile's last run are one.
	Notes []StateContextPrompt
}

// PersistedInput records a user input that PersistInput already stored.
type PersistedInput struct {
	// SessionTurn is the turn number of the run. It is read before the input is
	// appended, because the appended message itself would otherwise be counted.
	SessionTurn int64
	// Items is the number of input items that were stored.
	Items int
	// MessageOffset is the transcript byte offset of the stored user message,
	// the part of its web-ui message id after "session-user-o". HasMessage is
	// false when the input had no visible text.
	MessageOffset int64
	HasMessage    bool
	// history is the handle PersistInput read the history with. Run keeps using
	// it, so the first round expands only the records appended since.
	history *transcript.InputHistory
}

// PersistInput stores the user input of a run and repairs tool calls that a
// previous run left without a result. It is the first step of Run, exposed so a
// caller can make the input durable before the work that may still fail: the
// TypeScript backend appends the user message and only then loads the model,
// the tools and the credentials, so a failure there still leaves the message in
// the transcript. nextSessionTurn has the meaning of RunOptions.NextSessionTurn;
// nil reads the turn from the transcript.
func PersistInput(store *transcript.TurnStore, input json.RawMessage, nextSessionTurn func() (int64, error)) (*PersistedInput, error) {
	return PersistInputWithImages(store, input, nextSessionTurn, nil)
}

// PersistInputWithImages is PersistInput with an image store: an image of the user
// message that is a data URL is written to the store and the transcript keeps the
// stored-image reference, as for a tool-result image. A nil store keeps the data URL
// in the transcript, which is what PersistInput does.
func PersistInputWithImages(store *transcript.TurnStore, input json.RawMessage, nextSessionTurn func() (int64, error), images *imagestore.Store) (*PersistedInput, error) {
	if store == nil {
		return nil, errors.New("transcript turn store is required")
	}
	inputItems, err := NormalizeInput(input)
	if err != nil {
		return nil, err
	}
	for index, item := range inputItems {
		if err := transcript.ValidateInputItem(index, item); err != nil {
			return nil, err
		}
	}
	for index, item := range inputItems {
		inputItems[index], _ = images.Externalize(item)
	}
	history := store.NewInputHistory()
	if err := repairPendingToolCalls(store, history); err != nil {
		return nil, err
	}
	if nextSessionTurn == nil {
		nextSessionTurn = store.NextSessionTurn
	}
	sessionTurn, err := nextSessionTurn()
	if err != nil {
		return nil, fmt.Errorf("read next session turn: %w", err)
	}
	appended, err := store.AppendInputItemsAt(inputItems...)
	if err != nil {
		return nil, fmt.Errorf("persist session input: %w", err)
	}
	return &PersistedInput{
		SessionTurn:   sessionTurn,
		Items:         len(inputItems),
		MessageOffset: appended.MessageOffset,
		HasMessage:    appended.HasMessage,
		history:       history,
	}, nil
}

// RunResult contains the committed response envelope and transcript commit
// metadata. Event uses the replay-compatible response.completed outer type,
// including for an underlying response.incomplete event.
type RunResult struct {
	TurnID string
	Event  json.RawMessage
	Commit transcript.CommitResult
}

// Runner serializes runs for one session and composes transcript persistence
// with a provider-neutral Responses stream.
type Runner struct {
	store    *transcript.TurnStore
	streamer responses.Streamer
	settings ModelSettings
	mu       sync.Mutex
}

// NewRunner constructs a runner. It does not take ownership of store or streamer.
func NewRunner(store *transcript.TurnStore, streamer responses.Streamer, settings ModelSettings) (*Runner, error) {
	if store == nil {
		return nil, errors.New("transcript turn store is required")
	}
	if streamer == nil {
		return nil, errors.New("Responses stream client is required")
	}
	if err := validateModelSettings(settings); err != nil {
		return nil, err
	}
	settings.Request = cloneRaw(settings.Request)
	settings.HostedTools = cloneRawList(settings.HostedTools)
	return &Runner{store: store, streamer: streamer, settings: settings}, nil
}

// NormalizeInput converts the TypeScript session input shapes into persisted
// Responses API items. input must be a JSON-encoded string, object, or array.
func NormalizeInput(input json.RawMessage) ([][]byte, error) {
	if !json.Valid(input) {
		return nil, errors.New("session input must be valid JSON")
	}
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		return nil, errors.New("session input must be text or a Responses API input item")
	}

	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, fmt.Errorf("decode session text input: %w", err)
		}
		if strings.TrimSpace(text) == "" {
			return nil, errors.New("session input must not be empty")
		}
		item, err := json.Marshal(userInputMessage{
			Type:    "message",
			Role:    "user",
			Content: []inputTextContent{{Type: "input_text", Text: text}},
		})
		if err != nil {
			return nil, fmt.Errorf("serialize session text input: %w", err)
		}
		return [][]byte{item}, nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("decode session input item list: %w", err)
		}
		if len(items) == 0 {
			return nil, errors.New("session input item list must not be empty")
		}
		return copyRawItems(items), nil
	case '{':
		return [][]byte{cloneRaw(trimmed)}, nil
	default:
		return nil, errors.New("session input must be text or a Responses API input item")
	}
}

// BuildRequest merges raw model request fields with the current model, prompt,
// authoritative transcript history, and streaming mode.
func BuildRequest(settings ModelSettings, history [][]byte) (json.RawMessage, error) {
	return BuildRequestWithTools(settings, history, nil)
}

// BuildRequestWithTools adds the enabled native tool definitions when present.
// When no native tools are exposed, arbitrary model.request tools are retained.
func BuildRequestWithTools(settings ModelSettings, history [][]byte, responseTools []json.RawMessage) (json.RawMessage, error) {
	return BuildRequestWithStateContext(settings, history, responseTools, nil)
}

// BuildRequestWithStateContext inserts each derived-state block after the history item
// it follows, without persisting it into messages.jsonl. Blocks are in write order. A
// block that follows more items than the history holds is not sent.
func BuildRequestWithStateContext(settings ModelSettings, history [][]byte, responseTools []json.RawMessage, stateContext []StateContextPrompt) (json.RawMessage, error) {
	if err := validateModelSettings(settings); err != nil {
		return nil, err
	}

	input := make([]json.RawMessage, 0, len(history)+len(stateContext)+1)
	if settings.PromptText != "" {
		prompt, err := json.Marshal(promptMessage{
			Role:    settings.PromptRole,
			Content: []inputTextContent{{Type: "input_text", Text: settings.PromptText}},
		})
		if err != nil {
			return nil, fmt.Errorf("serialize configured prompt: %w", err)
		}
		input = append(input, prompt)
	}
	nextBlock := 0
	appendBlocks := func(after int) error {
		for ; nextBlock < len(stateContext) && stateContext[nextBlock].After <= after; nextBlock++ {
			block := stateContext[nextBlock]
			if block.Text == "" {
				continue
			}
			statePrompt, err := json.Marshal(promptMessage{
				Role:    block.Role,
				Content: []inputTextContent{{Type: "input_text", Text: block.Text}},
			})
			if err != nil {
				return fmt.Errorf("serialize state context prompt: %w", err)
			}
			input = append(input, statePrompt)
		}
		return nil
	}
	for index, item := range history {
		if err := appendBlocks(index); err != nil {
			return nil, err
		}
		if !isJSONObject(item) {
			return nil, fmt.Errorf("history item %d must be a JSON object", index)
		}
		// Not copied: json.Marshal below only reads the item.
		input = append(input, item)
	}
	if err := appendBlocks(len(history)); err != nil {
		return nil, err
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("serialize Responses input: %w", err)
	}

	requestFields := make(map[string]json.RawMessage)
	if len(settings.Request) > 0 {
		if err := json.Unmarshal(settings.Request, &requestFields); err != nil {
			return nil, fmt.Errorf("model request settings must be a JSON object: %w", err)
		}
		if requestFields == nil {
			return nil, errors.New("model request settings must be a JSON object")
		}
	}
	modelJSON, err := json.Marshal(settings.Model)
	if err != nil {
		return nil, fmt.Errorf("serialize model name: %w", err)
	}
	requestFields["model"] = modelJSON
	requestFields["input"] = inputJSON
	if settings.PromptCacheKey != "" {
		if requestFields["prompt_cache_key"], err = json.Marshal(settings.PromptCacheKey); err != nil {
			return nil, fmt.Errorf("serialize prompt cache key: %w", err)
		}
	}
	if len(settings.TextFormat) > 0 {
		if requestFields["text"], err = withTextFormat(requestFields["text"], settings.TextFormat); err != nil {
			return nil, err
		}
	}
	requestFields["stream"] = json.RawMessage("true")
	if allTools := appendHostedTools(responseTools, settings.HostedTools); len(allTools) > 0 {
		encodedTools, err := json.Marshal(allTools)
		if err != nil {
			return nil, fmt.Errorf("serialize Responses tools: %w", err)
		}
		requestFields["tools"] = encodedTools
	}

	request, err := encodeRequestFields(requestFields, "input")
	if err != nil {
		return nil, fmt.Errorf("serialize Responses request: %w", err)
	}
	return request, nil
}

// withTextFormat returns the request's text object with format set to format; the other
// fields of text (verbosity, for one) are kept.
func withTextFormat(text, format json.RawMessage) (json.RawMessage, error) {
	fields := make(map[string]json.RawMessage)
	if len(text) > 0 && !bytes.Equal(bytes.TrimSpace(text), []byte("null")) {
		if err := json.Unmarshal(text, &fields); err != nil || fields == nil {
			return nil, errors.New("model request text must be a JSON object to take an output format")
		}
	}
	fields["format"] = format
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("serialize text format: %w", err)
	}
	return encoded, nil
}

// encodeRequestFields writes fields as json.Marshal writes a map[string]json.RawMessage
// (keys sorted, every value compacted with HTML escaping), except the value of encoded, which
// json.Marshal already produced and so is written as is: compacting it again would only
// re-scan the whole history to produce the same bytes.
func encodeRequestFields(fields map[string]json.RawMessage, encoded string) ([]byte, error) {
	keys := make([]string, 0, len(fields))
	size := 2
	for key, value := range fields {
		keys = append(keys, key)
		size += len(key) + len(value) + 4
	}
	sort.Strings(keys)
	out := bytes.NewBuffer(make([]byte, 0, size))
	out.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			out.WriteByte(',')
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		out.Write(name)
		out.WriteByte(':')
		value := fields[key]
		if key != encoded {
			if value, err = json.Marshal(value); err != nil {
				return nil, err
			}
		}
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// Run appends user input, rebuilds history from messages.jsonl, and streams
// Responses provider rounds. It commits each completed or incomplete response;
// when a tool catalog is configured, it executes function calls, persists their
// outputs, and requests follow-on rounds up to the configured tool-round limit.
func (r *Runner) Run(ctx context.Context, input json.RawMessage, options RunOptions) (result RunResult, err error) {
	if r == nil || r.store == nil || r.streamer == nil {
		return RunResult{}, errors.New("session runner is not initialized")
	}
	if ctx == nil {
		return RunResult{}, errors.New("session context is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}
	trace := options.MemoryTrace.BeginRun()
	defer trace.Stop()

	// A message sent once the run returned is refused to its sender instead of being lost;
	// one accepted but never stored is reported.
	defer func() {
		if dropped := options.Steer.Close(); len(dropped) > 0 && options.OnSteerDropped != nil {
			options.OnSteerDropped(dropped)
		}
	}()
	// A spent budget refuses the run before its input is stored. A caller that stored the
	// input itself (PersistedInput) checked the budget before storing it; checking again
	// here could refuse a run whose message is already stored, leaving it unanswered.
	if options.PersistedInput == nil {
		if err := options.Budget.Check(time.Now()); err != nil {
			return RunResult{}, err
		}
	}
	persisted, err := r.persistRunInput(input, options)
	if err != nil {
		return RunResult{}, err
	}
	sessionTurn := persisted.SessionTurn
	trace.Mark("input-normalized", map[string]any{"input_items": persisted.Items})
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}

	result.TurnID, err = runTurnID(options.TurnID)
	if err != nil {
		return RunResult{}, err
	}
	if options.MaxToolRounds < 0 {
		return result, errors.New("maximum tool rounds cannot be negative")
	}
	// Off by default; when on, the run records the workspace before it changes it
	// and again when it ends, so the user can undo it. The end is recorded
	// whatever the outcome, like the TypeScript try/finally.
	checkpointRun := checkpoint.NewRun(options.Checkpoint, result.TurnID, sessionTurn)
	checkpointRun.Start(ctx)
	defer checkpointRun.Finish()
	// One handle per run: later rounds expand only the records appended since
	// the previous round instead of re-parsing the whole transcript. The handle
	// that PersistInput read the history with is reused, so the run start does
	// not parse the transcript twice.
	inputHistory := persisted.history
	if inputHistory == nil {
		inputHistory = r.store.NewInputHistory()
	}
	defer inputHistory.Release()
	// The run's own meter; it adds into the meter of the run that started this one.
	meter := NewCostMeter(CostMeterFrom(ctx))
	run := &runRounds{
		ctx:          WithCostMeter(ctx, meter),
		trace:        trace,
		checkpoint:   checkpointRun,
		inputHistory: inputHistory,
		options:      options,
		meter:        meter,
		sessionTurn:  sessionTurn,
		result:       result,
	}
	for round := 0; ; round++ {
		done, err := r.runRound(run, round)
		if err != nil {
			return run.result, err
		}
		if !done {
			continue
		}
		// The round a stop hook may add is a provider round like a tool round: with a cap
		// reached it could not start, so the hook (a command, which may have side effects)
		// is not run, and the run ends as it would without a hook.
		_, capped := runLimit(run.usedTokens, run.meter, run.options)
		if capped == nil && !run.declined && !options.Steer.hasPending() && run.stopContinues < options.StopMaxContinues && options.OnStop != nil {
			message, err := options.OnStop(run.ctx, run.result.TurnID, finalAnswerText(run.result.Event), run.stopContinues)
			if err != nil {
				return run.result, err
			}
			if message != "" {
				if err := r.storeStopMessage(message, options); err != nil {
					return run.result, err
				}
				run.stopContinues++
				continue
			}
		}
		// A steering message that is still waiting gets one more round, unless a cap is
		// reached (the message is then reported dropped).
		if options.Steer.closeIfEmpty() {
			return run.result, nil
		}
		if _, err := runLimit(run.usedTokens, run.meter, run.options); err != nil {
			return run.result, err
		}
	}
}

// runTurnID returns the caller's turn ID, or a new one when the caller gave none.
func runTurnID(turnID string) (string, error) {
	if turnID != "" {
		return turnID, nil
	}
	turnID, err := newTurnID()
	if err != nil {
		return "", fmt.Errorf("create response turn ID: %w", err)
	}
	return turnID, nil
}

// runRounds is the state one Run carries from one provider round to the next.
type runRounds struct {
	ctx          context.Context
	trace        memorytrace.Handle
	checkpoint   *checkpoint.Run
	inputHistory *transcript.InputHistory
	// options is the run's own copy: a refresh after automatic compaction replaces its
	// state context for the rounds that follow.
	options     RunOptions
	meter       *CostMeter
	sessionTurn int64
	// usedTokens is what the provider reported for the rounds of this run so far. Every
	// round re-sends the history, so this is what the run is billed, not the transcript size.
	usedTokens int64
	// budgetCost is the part of the meter's total already recorded in the budget.
	budgetCost float64
	result     RunResult
	// overflowAttempts counts the overflow compactions of this run.
	overflowAttempts int
	// stopContinues counts the rounds stop hooks added to this run.
	stopContinues int
	// declined is set when the user declined a draft, which ends the run whatever a stop
	// hook says.
	declined bool
}

// runRound sends one provider round and runs the function calls it returned. It reports
// done when the run ends without an error: the model called no tool, or a call declined
// to go on.
func (r *Runner) runRound(run *runRounds, round int) (done bool, err error) {
	ctx, options := run.ctx, &run.options
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// A declined draft ends the round it happened in, not the rounds a steering message
	// adds after it.
	run.declined = false
	if err := r.storeSteerMessages(*options); err != nil {
		return false, err
	}
	// The tools array is rebuilt every round: a lazy tool that load_tool loaded in the
	// previous round is listed in this one.
	var responseTools []json.RawMessage
	if options.ToolCatalog != nil {
		responseTools, err = options.ToolCatalog.ResponseTools()
		if err != nil {
			return false, fmt.Errorf("build Responses tool definitions: %w", err)
		}
	}
	// TypeScript suffixes every round, including the first, so the staging
	// directory of a response is always <turn>-<round>.
	responseTurnID := run.result.TurnID + "-" + strconv.Itoa(round)
	responseResult, err := r.sendRound(run, round, responseTurnID, responseTools)
	// A round the provider refused for being over the context window is sent again after
	// a compaction, while compaction.overflow allows and the compaction drops something.
	for err != nil && r.overflowRetryAllowed(ctx, err, *options, run.overflowAttempts) {
		run.overflowAttempts++
		compacted, compactErr := r.compactForOverflow(ctx, run)
		if overflow := options.AutomaticCompaction.Overflow; overflow.OnCompacted != nil {
			overflow.OnCompacted(run.overflowAttempts, compacted, err)
		}
		if compactErr != nil {
			return false, errors.Join(err, compactErr)
		}
		if !compacted {
			break
		}
		responseResult, err = r.sendRound(run, round, responseTurnID+"-overflow-"+strconv.Itoa(run.overflowAttempts), responseTools)
	}
	if err != nil {
		return false, err
	}
	run.result.Event = responseResult.Event
	run.result.Commit = responseResult.Commit
	if run.trace.Enabled() {
		// The details decode the whole event; a disabled trace would throw them away.
		run.trace.Mark("provider-response-complete", responseTraceDetails(run.result.Event))
	}
	tokens, calls, err := responseTokensAndCalls(run.result.Event)
	run.usedTokens += tokens
	if err != nil {
		return false, err
	}
	if err := r.recordBudget(run, tokens); err != nil {
		return false, err
	}
	refreshStateContext, err := r.compactAfterResponse(ctx, run.result.Event, calls, *options)
	if err != nil {
		return false, err
	}
	refreshAfterCompaction := func() error {
		if !refreshStateContext || options.RefreshStateContext == nil {
			return nil
		}
		stateContext, err := options.RefreshStateContext(ctx)
		if err != nil {
			return fmt.Errorf("refresh state context after automatic compaction: %w", err)
		}
		options.StateContext = stateContext
		return nil
	}
	if len(calls) == 0 || options.ToolCatalog == nil {
		return true, refreshAfterCompaction()
	}
	if err := r.enforceToolLimits(calls, round, run.usedTokens, run.meter, *options, refreshAfterCompaction); err != nil {
		return false, err
	}
	declined, err := r.executeToolCalls(ctx, run.trace, run.checkpoint, calls, round, *options, refreshAfterCompaction)
	if err != nil {
		return false, err
	}
	run.declined = declined
	return declined, nil
}

// sendRound builds the request of one provider round from the current history and sends it.
func (r *Runner) sendRound(run *runRounds, round int, responseTurnID string, responseTools []json.RawMessage) (RunResult, error) {
	request, err := r.buildRoundRequest(run.ctx, run.trace, run.inputHistory, run.options, responseTools)
	if err != nil {
		return RunResult{}, err
	}
	roundCtx, roundSpan := tracing.StartModelRequest(run.ctx, r.settings.Model, round)
	responseResult, err := r.runResponse(roundCtx, request, run.sessionTurn, int64(round+1), responseTurnID, run.options)
	tracing.EndModelRequest(roundSpan, responseResult.Event, err)
	return responseResult, err
}

// overflowRetryAllowed reports whether err is the provider refusing a request for being
// over the context window and the run may still compact for it.
func (r *Runner) overflowRetryAllowed(ctx context.Context, err error, options RunOptions, attempts int) bool {
	settings := options.AutomaticCompaction
	if settings == nil || !settings.Enabled || settings.Overflow == nil || attempts >= settings.Overflow.MaxAttempts {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return IsContextOverflow(err, settings.Overflow.Patterns)
}

// IsContextOverflow reports whether the provider's error contains one of patterns, ignoring
// case. For a stream that ended with response.failed or an error event, only the provider's
// error code and message are matched, not the response it echoed (its instructions and
// tools may name a pattern); any other error is matched by its text.
func IsContextOverflow(err error, patterns []string) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	var failed *ResponseFailedError
	if errors.As(err, &failed) {
		text = failed.Detail
	}
	text = strings.ToLower(text)
	for _, pattern := range patterns {
		if pattern = strings.ToLower(strings.TrimSpace(pattern)); pattern != "" && strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

// compactForOverflow compacts the history with the automatic compaction settings, keeping
// the latest turn whole (the run's own calls and results stay, so the model does not lose
// the work it is doing), and refreshes the state context when the transcript was
// rewritten. When only that turn is left, nothing is compacted and the provider's error
// stands.
func (r *Runner) compactForOverflow(ctx context.Context, run *runRounds) (bool, error) {
	compactionOptions := r.automaticCompactionOptions(ctx, run.options)
	compactionOptions.KeepLatestTurn = true
	compacted, err := r.compactAutomatically(compactionOptions, run.options)
	if err != nil || !compacted {
		return false, err
	}
	if run.options.RefreshStateContext != nil {
		stateContext, err := run.options.RefreshStateContext(ctx)
		if err != nil {
			return true, fmt.Errorf("refresh state context after overflow compaction: %w", err)
		}
		run.options.StateContext = stateContext
	}
	return true, nil
}

// persistRunInput stores the user input unless the caller already persisted it,
// then reports the accepted input to OnInputAccepted.
func (r *Runner) persistRunInput(input json.RawMessage, options RunOptions) (*PersistedInput, error) {
	if options.PersistedInput != nil {
		return options.PersistedInput, nil
	}
	persisted, err := PersistInputWithImages(r.store, input, options.NextSessionTurn, options.ImageStore)
	if err != nil {
		return nil, err
	}
	if options.OnInputAccepted != nil {
		options.OnInputAccepted(*persisted)
	}
	return persisted, nil
}

// buildRoundRequest reads the history for one provider round and builds the request
// with the images sent as their resized copies.
func (r *Runner) buildRoundRequest(ctx context.Context, trace memorytrace.Handle, inputHistory *transcript.InputHistory, options RunOptions, responseTools []json.RawMessage) (json.RawMessage, error) {
	history, err := inputHistory.Read()
	if err != nil {
		return nil, fmt.Errorf("read session history: %w", err)
	}
	historyDetails := map[string]any{"history_items": len(history)}
	if trace.Enabled() {
		if transcriptBytes, err := r.store.TranscriptBytes(); err == nil {
			historyDetails["transcript_bytes"] = transcriptBytes
		}
	}
	trace.Mark("history-loaded", historyDetails)
	// References come back as data URLs first, so the resize step sees real bytes.
	sentHistory, restoreStats := options.ImageStore.Rehydrate(history)
	sentHistory, resizeStats := options.ImageResizer.Apply(sentHistory, options.ToolCatalog.SourceOf)
	settings := r.settings
	if len(options.OutputFormat) > 0 {
		settings.TextFormat = options.OutputFormat
	}
	stateContext := options.StateContext
	for _, note := range options.Notes {
		if note.Text == "" {
			continue
		}
		// The copy keeps the caller's slice as it is for the next round.
		note.After = len(sentHistory)
		stateContext = append(stateContext[:len(stateContext):len(stateContext)], note)
	}
	if options.PlanPrompt.Text != "" {
		// Last, after the history and every state-context block: the copy keeps the
		// caller's slice as it is for the next round.
		plan := options.PlanPrompt
		plan.After = len(sentHistory)
		stateContext = append(stateContext[:len(stateContext):len(stateContext)], plan)
	}
	request, err := BuildRequestWithStateContext(settings, sentHistory, responseTools, stateContext)
	if err != nil {
		return nil, fmt.Errorf("build Responses request: %w", err)
	}
	trace.Mark("request-built", map[string]any{
		"history_items":     len(history),
		"has_state_context": len(options.StateContext) > 0,
		"plan":              options.PlanPrompt.Text != "",
		"notes":             len(options.Notes),
		"tool_count":        len(responseTools),
		"images_resized":    resizeStats.Resized,
		"image_bytes_saved": resizeStats.BytesBefore - resizeStats.BytesAfter,
		"images_restored":   restoreStats.Restored,
		"images_missing":    restoreStats.Missing,
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return request, nil
}

// compactAfterResponse compacts the transcript when the automatic compaction trigger
// fires for the response. It reports whether the state context must be refreshed.
func (r *Runner) compactAfterResponse(ctx context.Context, event json.RawMessage, calls []json.RawMessage, options RunOptions) (bool, error) {
	responseID, shouldCompact := automaticCompactionTrigger(event, options.AutomaticCompaction)
	if !shouldCompact {
		return false, nil
	}
	compactionOptions := r.automaticCompactionOptions(ctx, options)
	if len(calls) > 0 && responseID != "" {
		compactionOptions.PreserveResponseID = responseID
	}
	return r.compactAutomatically(compactionOptions, options)
}

// automaticCompactionOptions returns the compaction options of the run's automatic
// compaction settings, with the summary callback when compaction.summary is on.
func (r *Runner) automaticCompactionOptions(ctx context.Context, options RunOptions) transcript.CompactionOptions {
	settings := options.AutomaticCompaction
	compactionOptions := transcript.CompactionOptions{
		TurnType:       settings.TurnType,
		KeepTurns:      settings.KeepTurns,
		KeepToolPairs:  settings.KeepToolPairs,
		BackupMode:     settings.BackupMode,
		BackupMinTurns: settings.BackupMinTurns,
	}
	if settings.Summary != nil {
		compactionOptions.Summarize = NewCompactionSummarizer(ctx, r.streamer, r.settings, *settings.Summary, settings.OnSummaryError)
	}
	return compactionOptions
}

// compactAutomatically runs one automatic compaction through the caller's CompactHistory
// (or the store's) and reports whether it rewrote the transcript.
func (r *Runner) compactAutomatically(compactionOptions transcript.CompactionOptions, options RunOptions) (bool, error) {
	compact := r.store.CompactHistory
	if options.CompactHistory != nil {
		compact = options.CompactHistory
	}
	compacted, err := compact(compactionOptions)
	if err != nil {
		return false, fmt.Errorf("automatically compact session transcript: %w", err)
	}
	if compacted.Compacted && options.OnUsageChanged != nil {
		options.OnUsageChanged()
	}
	return compacted.Compacted, nil
}

// enforceToolLimits refuses to start the calls of a round when the tool-round limit or
// the run token cap is reached. The calls get a result so the transcript stays valid.
func (r *Runner) enforceToolLimits(calls []json.RawMessage, round int, usedTokens int64, meter *CostMeter, options RunOptions, refreshAfterCompaction func() error) error {
	if round >= options.MaxToolRounds {
		summary := fmt.Sprintf("Tool execution was not started because the %d-round limit was reached.", options.MaxToolRounds)
		if _, err := appendToolOutputs(r.store, calls, nil, "tool_round_limit", summary); err != nil {
			return err
		}
		if err := refreshAfterCompaction(); err != nil {
			return err
		}
		return fmt.Errorf("tool execution exceeded %d tool rounds", options.MaxToolRounds)
	}
	// The cap is checked here, between rounds, from usage already reported: a
	// round in flight is never cut, but the next one is not started. The calls
	// get a result so the transcript stays a valid history for a later run.
	code, limit := runLimit(usedTokens, meter, options)
	if limit == nil {
		return nil
	}
	if code == "" {
		return limit
	}
	if _, err := appendToolOutputs(r.store, calls, nil, code, "Tool execution was not started. "+limit.Error()); err != nil {
		return err
	}
	if err := refreshAfterCompaction(); err != nil {
		return err
	}
	return limit
}

// runLimit returns the cap the run has reached (max_run_tokens, max_run_cost_usd, the
// budget) with the status code its unstarted calls get, or nil. A budget that fails for
// another reason returns that error with no code.
func runLimit(usedTokens int64, meter *CostMeter, options RunOptions) (string, error) {
	if options.MaxRunTokens > 0 && usedTokens >= options.MaxRunTokens {
		return "run_token_limit", &RunTokenLimitError{Used: usedTokens, Limit: options.MaxRunTokens}
	}
	if cost, _ := meter.Total(); options.MaxRunCostUSD > 0 && cost >= options.MaxRunCostUSD {
		return "run_cost_limit", &RunCostLimitError{Used: cost, Limit: options.MaxRunCostUSD}
	}
	if err := options.Budget.Check(time.Now()); err != nil {
		var limit *BudgetLimitError
		if !errors.As(err, &limit) {
			return "", err
		}
		return "budget_limit", limit
	}
	return "", nil
}

// recordBudget adds the tokens of the round's response and what the run's meter counted
// since the last record (the response, and a compaction summary written for the run) to
// the session's budget.
func (r *Runner) recordBudget(run *runRounds, tokens int64) error {
	if run.options.Budget == nil {
		return nil
	}
	cost, _ := run.meter.Total()
	delta := cost - run.budgetCost
	run.budgetCost = cost
	if err := run.options.Budget.Record(tokens, delta, time.Now()); err != nil {
		return fmt.Errorf("count the round in the session budget: %w", err)
	}
	return nil
}

// executeToolCalls runs the calls of one round, persists their outputs and reports
// whether the user declined a draft, which ends the run.
func (r *Runner) executeToolCalls(ctx context.Context, trace memorytrace.Handle, checkpointRun *checkpoint.Run, calls []json.RawMessage, round int, options RunOptions, refreshAfterCompaction func() error) (bool, error) {
	// Takes the checkpoint once, just before the first call that can change
	// files. A user-decision tool runs alone, so only its call counts then.
	checkpointRun.BeforeCalls(ctx, roundChangesWorkspace(options.ToolCatalog, calls, checkpointRun))
	// Remember how a user-decision tool was answered: a decline, a timeout or a
	// cancellation ends the run once its result is persisted, because the model
	// must not carry on as if the user had approved the draft.
	var decisionAnswer decisionRecorder
	toolExecution := options.ToolExecution
	resolved := toolExecution.OnInteractionResolved
	toolExecution.OnInteractionResolved = func(identity tools.InteractionIdentity, answer tools.InteractionAnswer, proposed json.RawMessage) error {
		decisionAnswer.record(answer)
		if resolved == nil {
			return nil
		}
		return resolved(identity, answer, proposed)
	}
	// A result is published the moment its call settles; the ids published
	// here are skipped by the pass after persistence below.
	toolRun, executionErr := executeToolRound(ctx, options.ToolCatalog, calls, toolExecution, options.MaxParallelToolCalls, options.ToolCallScheduling, options.LeafBudget, func(output json.RawMessage) {
		if options.OnToolResult != nil {
			options.OnToolResult(output)
		}
	})
	completed := toolRun.Completed
	errorCode := "tool_execution_failed"
	errorSummary := "Tool execution failed before every call produced a persisted result."
	if errors.Is(executionErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		errorCode = "tool_execution_cancelled"
		errorSummary = "Tool execution was cancelled before every call produced a persisted result."
	}
	// Large images go to the store first; the transcript keeps a reference. The live
	// stream above already carried the original result.
	persisted, _ := options.ImageStore.ExternalizeAll(completed)
	outputs, err := appendToolOutputs(r.store, calls, persisted, errorCode, errorSummary)
	if err != nil {
		return false, err
	}
	trace.Mark("tool-results-persisted", map[string]any{"round": round, "tool_calls": len(calls), "tool_results": len(outputs)})
	if options.OnToolResult != nil {
		for _, output := range outputs {
			if _, published := toolRun.Published[functionCallID(output)]; published {
				continue
			}
			options.OnToolResult(cloneRaw(output))
		}
	}
	if err := refreshAfterCompaction(); err != nil {
		return false, err
	}
	if executionErr != nil {
		return false, executionErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return decisionAnswer.declined(), nil
}

func responseTraceDetails(raw json.RawMessage) map[string]any {
	var envelope struct {
		Response struct {
			Output []json.RawMessage `json:"output"`
			Usage  struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	details := map[string]any{"output_items": len(envelope.Response.Output)}
	if envelope.Response.Usage.InputTokens != 0 {
		details["input_tokens"] = envelope.Response.Usage.InputTokens
	}
	if envelope.Response.Usage.OutputTokens != 0 {
		details["output_tokens"] = envelope.Response.Usage.OutputTokens
	}
	return details
}

func automaticCompactionTrigger(event []byte, settings *AutomaticCompactionSettings) (string, bool) {
	if settings == nil || !settings.Enabled || settings.TriggerTokens < 1 {
		return "", false
	}
	eventFields, err := decodeJSONObject(event)
	if err != nil {
		return "", false
	}
	responseRaw, exists := rawField(eventFields, "response")
	if !exists {
		return "", false
	}
	responseFields, err := decodeJSONObject(responseRaw)
	if err != nil {
		return "", false
	}
	statusRaw, exists := rawField(responseFields, "status")
	if !exists {
		return "", false
	}
	var status string
	if json.Unmarshal(statusRaw, &status) != nil || status != "completed" {
		return "", false
	}
	usageRaw, exists := rawField(responseFields, "usage")
	if !exists {
		return "", false
	}
	usageFields, err := decodeJSONObject(usageRaw)
	if err != nil {
		return "", false
	}
	inputTokensRaw, exists := rawField(usageFields, "input_tokens")
	if !exists {
		return "", false
	}
	inputTokens, ok := jsonNumberFloat(inputTokensRaw)
	if !ok || inputTokens < float64(settings.TriggerTokens) {
		return "", false
	}
	var responseID string
	if idRaw, exists := rawField(responseFields, "id"); exists {
		_ = json.Unmarshal(idRaw, &responseID)
	}
	return responseID, true
}

func jsonNumberFloat(raw []byte) (float64, bool) {
	if !isJSONNumber(raw) {
		return 0, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, false
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Float64()
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

func validateModelSettings(settings ModelSettings) error {
	if strings.TrimSpace(settings.Model) == "" {
		return errors.New("model name is required")
	}
	if len(settings.Request) > 0 {
		if _, err := decodeJSONObject(settings.Request); err != nil {
			return fmt.Errorf("model request settings must be a JSON object: %w", err)
		}
	}
	if err := validateHostedTools(settings.HostedTools); err != nil {
		return err
	}
	if settings.PromptText != "" && settings.PromptRole != "system" && settings.PromptRole != "developer" {
		return errors.New("prompt role must be system or developer when prompt text is set")
	}
	return nil
}

func newTurnID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

type rawObjectField struct {
	key   string
	value json.RawMessage
}

func decodeJSONObject(raw []byte) ([]rawObjectField, error) {
	if !json.Valid(raw) {
		return nil, errors.New("expected a valid JSON object")
	}
	return decodeValidJSONObject(raw)
}

// decodeValidJSONObject is decodeJSONObject for raw the caller has already checked with
// json.Valid, so the per-event path does not scan every stream event twice. Each value is a
// fresh json.RawMessage (Decode copies into it), so it is kept without another copy.
func decodeValidJSONObject(raw []byte) ([]rawObjectField, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return nil, errors.New("expected a JSON object")
	}

	fields := make([]rawObjectField, 0)
	positions := make(map[string]int)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("JSON object key must be a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if index, exists := positions[key]; exists {
			// JSON.parse keeps the first property position but the last value.
			fields[index].value = value
		} else {
			positions[key] = len(fields)
			fields = append(fields, rawObjectField{key: key, value: value})
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("unexpected data after JSON object")
		}
		return nil, err
	}
	return fields, nil
}

func rawField(fields []rawObjectField, key string) (json.RawMessage, bool) {
	for _, field := range fields {
		if field.key == key {
			return field.value, true
		}
	}
	return nil, false
}

func encodeJSONObject(fields []rawObjectField) ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for index, field := range fields {
		if index > 0 {
			out.WriteByte(',')
		}
		key, err := json.Marshal(field.key)
		if err != nil {
			return nil, err
		}
		out.Write(key)
		out.WriteByte(':')
		if !json.Valid(field.value) {
			return nil, fmt.Errorf("field %q does not contain valid JSON", field.key)
		}
		out.Write(field.value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

func makeCompletedEvent(raw []byte, sessionTurn, sessionStep int64, staged map[int64]json.RawMessage) ([]byte, error) {
	eventFields, err := decodeJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("completed Responses event must be an object: %w", err)
	}
	responseRaw, exists := rawField(eventFields, "response")
	if !exists {
		return nil, errors.New("completed response must be an object")
	}
	responseFields, err := decodeJSONObject(responseRaw)
	if err != nil {
		return nil, errors.New("completed response must be an object")
	}

	if len(staged) > 0 {
		stagedOutput, err := marshalStagedOutput(staged)
		if err != nil {
			return nil, err
		}
		outputRaw, hasOutput := rawField(responseFields, "output")
		rebuildOutput := !hasOutput
		if hasOutput {
			var output []json.RawMessage
			if err := json.Unmarshal(outputRaw, &output); err == nil && output != nil && len(output) == 0 {
				rebuildOutput = true
			}
		}
		if rebuildOutput {
			replaced := false
			for index := range responseFields {
				if responseFields[index].key == "output" {
					responseFields[index].value = stagedOutput
					replaced = true
					break
				}
			}
			if !replaced {
				responseFields = append(responseFields, rawObjectField{key: "output", value: stagedOutput})
			}
			responseRaw, err = encodeJSONObject(responseFields)
			if err != nil {
				return nil, fmt.Errorf("rebuild completed response output: %w", err)
			}
		}
	}

	envelope := struct {
		Type        string          `json:"type"`
		SessionTurn int64           `json:"session_turn"`
		SessionStep int64           `json:"session_step"`
		Response    json.RawMessage `json:"response"`
	}{Type: "response.completed", SessionTurn: sessionTurn, SessionStep: sessionStep, Response: responseRaw}
	completed, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("serialize completed response envelope: %w", err)
	}
	return completed, nil
}

func marshalStagedOutput(staged map[int64]json.RawMessage) (json.RawMessage, error) {
	indexes := make([]int64, 0, len(staged))
	for index := range staged {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(left, right int) bool { return indexes[left] < indexes[right] })
	items := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		items = append(items, cloneRaw(staged[index]))
	}
	output, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("serialize staged response output: %w", err)
	}
	return output, nil
}

// ResponseFailedError is a stream that ended with response.failed or an error event
// instead of response.completed.
type ResponseFailedError struct {
	message string
	// Detail is the provider's own error: the code and message of response.error, of a
	// top-level error, or of the error event itself. It is empty when the event has none.
	Detail string
}

func (e *ResponseFailedError) Error() string { return e.message }

func terminalFailureError(raw []byte, eventType string) error {
	const prefix = "Responses stream terminated without response.completed: "
	fields, err := decodeJSONObject(raw)
	if err != nil {
		return &ResponseFailedError{message: prefix + string(raw)}
	}
	details, exists := rawField(fields, "response")
	if !exists {
		details, exists = rawField(fields, "error")
	}
	if !exists {
		details, _ = json.Marshal(eventType)
	}
	return &ResponseFailedError{message: prefix + string(details), Detail: failureDetail(fields)}
}

// failureDetail returns the code and message of a failed stream event's error: response.error,
// then a top-level error (an object or a string), then the event's own code and message.
func failureDetail(fields []rawObjectField) string {
	if response, exists := rawField(fields, "response"); exists {
		if responseFields, err := decodeJSONObject(response); err == nil {
			if errorRaw, exists := rawField(responseFields, "error"); exists {
				if detail := errorObjectDetail(errorRaw); detail != "" {
					return detail
				}
			}
		}
	}
	if errorRaw, exists := rawField(fields, "error"); exists {
		if detail := errorObjectDetail(errorRaw); detail != "" {
			return detail
		}
	}
	return errorFieldsDetail(fields)
}

// errorObjectDetail returns "<code> <message>" of an error object, or the error itself
// when it is a string.
func errorObjectDetail(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	fields, err := decodeJSONObject(raw)
	if err != nil {
		return ""
	}
	return errorFieldsDetail(fields)
}

func errorFieldsDetail(fields []rawObjectField) string {
	parts := make([]string, 0, 3)
	for _, key := range []string{"type", "code", "message"} {
		raw, exists := rawField(fields, key)
		if !exists {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil && value != "" && !(key == "type" && (value == "error" || strings.HasPrefix(value, "response."))) {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " ")
}

func parseSafeOutputIndex(raw []byte) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, errors.New("output_index must be a non-negative safe integer")
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("output_index must be a non-negative safe integer")
	}
	floatValue, err := number.Float64()
	if err != nil || math.IsNaN(floatValue) || math.IsInf(floatValue, 0) || floatValue < 0 || math.Trunc(floatValue) != floatValue || floatValue > float64(maxSafeJSONInteger) {
		return 0, errors.New("output_index must be a non-negative safe integer")
	}
	return int64(floatValue), nil
}

func isJSONNumber(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	first := trimmed[0]
	return first == '-' || first >= '0' && first <= '9'
}

func isJSONObject(raw []byte) bool {
	if !json.Valid(raw) {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func cloneRaw(raw []byte) []byte {
	return append([]byte(nil), raw...)
}

func copyRawItems(items []json.RawMessage) [][]byte {
	copies := make([][]byte, len(items))
	for index, item := range items {
		copies[index] = cloneRaw(item)
	}
	return copies
}

// decisionRecorder keeps the first negative answer (decline, timeout, cancellation) so a later accept never overwrites it; otherwise the last answer wins.
type decisionRecorder struct {
	mu     sync.Mutex
	answer *tools.InteractionAnswer
}

func isNegativeAnswer(answer tools.InteractionAnswer) bool {
	return answer.Decision == tools.InteractionDecline || answer.Outcome != tools.InteractionAnswered
}

func (d *decisionRecorder) record(answer tools.InteractionAnswer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.answer != nil && isNegativeAnswer(*d.answer) {
		return
	}
	d.answer = &answer
}

func (d *decisionRecorder) declined() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.answer != nil && isNegativeAnswer(*d.answer)
}
