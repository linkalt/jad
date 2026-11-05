package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"jad/backend-go/internal/sessionfs"
)

const (
	HookEventRunStart = "run_start"
	HookEventRunEnd   = "run_end"
	// HookEventPreCompact fires before a compaction attempt; its commands are reported to
	// the server log, and a blocking one that does not exit 0 skips the attempt.
	HookEventPreCompact = "pre_compact"
	// HookEventPromptSubmit fires before the input of a run is stored; a blocking command
	// that does not exit 0 refuses the run, and a context command adds its output for the model.
	HookEventPromptSubmit = "prompt_submit"
	// HookEventSubagentStop fires in the parent session when a sub-agent run ends.
	HookEventSubagentStop = "subagent_stop"
	// HookEventSessionStart fires before the first run of a conversation; a context command
	// adds its output for the model, like prompt_submit's.
	HookEventSessionStart = "session_start"
	// HookEventStop fires when a run is about to end without an error; a command may ask
	// the run to go on.
	HookEventStop = "stop"
	// HookEventNotification fires when a run waits for the user.
	HookEventNotification = "notification"

	// NotificationInteraction is the Trigger of a notification fired because a tool asks
	// the user a question.
	NotificationInteraction = "interaction"

	CompactTriggerAutomatic = "automatic"
	CompactTriggerManual    = "manual"

	RunOutcomeOK        = "ok"
	RunOutcomeError     = "error"
	RunOutcomeCancelled = "cancelled"
)

// RunEvent is the JSON a run_start, run_end, pre_compact, prompt_submit or subagent_stop
// command reads on stdin. Outcome and Error are set for run_end and subagent_stop; Trigger
// for pre_compact; Prompt for prompt_submit; Agent, CallID, Rounds, Tokens and Answer for
// subagent_stop.
type RunEvent struct {
	Event         string `json:"event"`
	Session       string `json:"session"`
	Profile       string `json:"profile"`
	TurnID        string `json:"turn_id,omitempty"`
	Trigger       string `json:"trigger,omitempty"`
	WorkspaceRoot string `json:"workspace_root"`
	Prompt        string `json:"prompt,omitempty"`
	PromptCut     bool   `json:"prompt_cut,omitempty"`
	Agent         string `json:"agent,omitempty"`
	CallID        string `json:"call_id,omitempty"`
	Rounds        int    `json:"rounds,omitempty"`
	Tokens        int64  `json:"tokens,omitempty"`
	Answer        string `json:"answer,omitempty"`
	AnswerCut     bool   `json:"answer_cut,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	Error         string `json:"error,omitempty"`
	ErrorCut      bool   `json:"error_cut,omitempty"`
	// Tool and Title are set for a notification about a tool's question.
	Tool  string `json:"tool,omitempty"`
	Title string `json:"title,omitempty"`
	// Continues is how many times stop commands already made this run go on.
	Continues int `json:"continues,omitempty"`
}

// RunEventResult is what the commands of one event had to say.
type RunEventResult struct {
	// Reports holds one section per command that has something to say, for the server log.
	Reports []string
	// BlockedBy names the blocking command that did not exit 0; the commands after it did not start.
	BlockedBy string
	// Context holds the standard output of each context command that exited 0 and printed
	// something, in order, each under its command's name.
	Context []HookContext
	// Prompt is the prompt a prompt_submit rewrite command gave, when one did; the last
	// one wins. RewrittenBy names that command.
	Prompt      *string
	RewrittenBy string
	// Continue is the message a stop command asked the run to go on with; the first
	// command that asks wins and the later ones do not start. ContinuedBy names it.
	Continue    string
	ContinuedBy string
}

// HookContext is the standard output of one prompt_submit context command.
type HookContext struct {
	Hook string
	Text string
	Cut  bool
}

// HookBlockedError is the error of a run or a compaction a blocking hook stopped. Report
// is the hooks' report, for the server log.
type HookBlockedError struct {
	Event  string
	Hook   string
	Report string
}

func (e *HookBlockedError) Error() string {
	switch e.Event {
	case HookEventPromptSubmit:
		return fmt.Sprintf("the prompt_submit hook %s blocked the prompt; nothing was stored", e.Hook)
	case HookEventPreCompact:
		return fmt.Sprintf("the pre_compact hook %s blocked the compaction", e.Hook)
	}
	return fmt.Sprintf("the %s hook %s blocked the action", e.Event, e.Hook)
}

// ClientMessage is the text a client may read: it names only the event and the hook,
// both from the session's own settings, and leaves the hook's output to the server log.
func (e *HookBlockedError) ClientMessage() string {
	message := e.Error()
	return strings.ToUpper(message[:1]) + message[1:] + "."
}

// FireRunEvent runs the commands of the event in order and returns one report section per
// command that has something to say. The reports are for the server log: they never reach
// the model or the transcript, and a command never changes the run's result.
func FireRunEvent(ctx context.Context, hooks sessionfs.HooksSettings, apiKeyEnv string, event RunEvent) []string {
	return FireRunEventResult(ctx, hooks, apiKeyEnv, event).Reports
}

// FireRunEventResult is FireRunEvent that also says which blocking command stopped the
// event and what the context commands printed. Only prompt_submit and pre_compact entries
// can be blocking, and only prompt_submit entries can give context; the caller decides
// what a block or a context means.
func FireRunEventResult(ctx context.Context, hooks sessionfs.HooksSettings, apiKeyEnv string, event RunEvent) RunEventResult {
	if !hooks.RunEventsActive() {
		return RunEventResult{}
	}
	var list []sessionfs.RunEventHook
	switch event.Event {
	case HookEventRunStart:
		list = hooks.RunStart
	case HookEventRunEnd:
		list = hooks.RunEnd
	case HookEventPreCompact:
		list = hooks.PreCompact
	case HookEventPromptSubmit:
		list = hooks.PromptSubmit
	case HookEventSubagentStop:
		list = hooks.SubagentStop
	case HookEventSessionStart:
		list = hooks.SessionStart
	case HookEventStop:
		list = hooks.Stop
	case HookEventNotification:
		list = hooks.Notification
	}
	if len(list) == 0 {
		return RunEventResult{}
	}
	payload := encodeRunEvent(event, int(hooks.MaxPayloadBytes))
	var result RunEventResult
	for _, hook := range list {
		section, passed, stdout := runHookChildOutput(ctx, hooks, apiKeyEnv, event.WorkspaceRoot, hookChild{
			name:          hook.Name,
			setting:       "hooks." + event.Event + "[" + hook.Name + "].argv",
			argv:          hook.Argv,
			cwd:           event.WorkspaceRoot,
			stdin:         payload,
			onFailureOnly: hook.OnFailureOnly,
			// An answer read as JSON keeps its long lines whole.
			rawStdout: hook.Rewrite || event.Event == HookEventStop,
		})
		if section != "" {
			result.Reports = append(result.Reports, section)
		}
		if hook.Blocking && !passed {
			result.BlockedBy = hook.Name
			break
		}
		if hook.Context && passed {
			if text := strings.TrimSpace(stdout.text); text != "" {
				result.Context = append(result.Context, HookContext{Hook: hook.Name, Text: text, Cut: stdout.truncated})
			}
		}
		if hook.Rewrite && passed && event.Event == HookEventPromptSubmit {
			prompt, rewritten, err := decodePromptRewrite(stdout)
			if err != nil {
				// A rewrite the run cannot honour refuses it, as a blocking command would.
				result.Reports = append(result.Reports, "[hook "+hook.Name+"] rewrite refused: "+err.Error())
				result.BlockedBy = hook.Name
				break
			}
			if rewritten {
				result.Prompt, result.RewrittenBy = &prompt, hook.Name
			}
		}
		if event.Event == HookEventStop && passed {
			if message := decodeStopContinue(stdout); message != "" {
				result.Continue, result.ContinuedBy = message, hook.Name
				break
			}
		}
	}
	return result
}

// decodePromptRewrite reads {"prompt":"..."} from a rewrite command's standard output.
// Empty output changes nothing; anything else that is not such an object, a cut output
// or an empty prompt is an error.
func decodePromptRewrite(stdout bashStreamOutput) (string, bool, error) {
	text := strings.TrimSpace(stdout.text)
	if text == "" {
		return "", false, nil
	}
	if stdout.truncated {
		return "", false, fmt.Errorf("the output was cut at hooks.max_output_bytes, so the new prompt is incomplete")
	}
	var decoded struct {
		Prompt *string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(text), &decoded); err != nil || decoded.Prompt == nil {
		return "", false, fmt.Errorf(`the output must be empty or a JSON object {"prompt":"..."}`)
	}
	if strings.TrimSpace(*decoded.Prompt) == "" {
		return "", false, fmt.Errorf("the new prompt is empty")
	}
	return *decoded.Prompt, true, nil
}

// decodeStopContinue reads {"continue":"<message>"} from a stop command's standard
// output; anything else (empty output, other text, an empty message) lets the run end.
func decodeStopContinue(stdout bashStreamOutput) string {
	text := strings.TrimSpace(stdout.text)
	if text == "" || stdout.truncated {
		return ""
	}
	var decoded struct {
		Continue string `json:"continue"`
	}
	if json.Unmarshal([]byte(text), &decoded) != nil {
		return ""
	}
	return strings.TrimSpace(decoded.Continue)
}

// encodeRunEvent writes the event within maximum bytes: the error, the prompt and the answer
// each get a share of it, and are cut again until the whole event fits.
func encodeRunEvent(event RunEvent, maximum int) string {
	event.Error, event.ErrorCut = cutUTF8(event.Error, int64(maximum/2))
	event.Prompt, event.PromptCut = cutUTF8(event.Prompt, int64(maximum/2))
	event.Answer, event.AnswerCut = cutUTF8(event.Answer, int64(maximum/2))
	for {
		encoded, err := json.Marshal(event)
		if err != nil {
			return ""
		}
		if len(encoded) <= maximum {
			return string(encoded)
		}
		// JSON escapes make the text longer than the bytes counted above: cut the longest
		// field in proportion until the event fits.
		field, cut := &event.Prompt, &event.PromptCut
		if len(event.Answer) > len(*field) {
			field, cut = &event.Answer, &event.AnswerCut
		}
		if len(event.Error) > len(*field) {
			field, cut = &event.Error, &event.ErrorCut
		}
		if *field == "" {
			return string(encoded)
		}
		smaller := int64(len(*field)) * int64(maximum) / int64(len(encoded))
		if smaller >= int64(len(*field)) {
			smaller = int64(len(*field)) - 1
		}
		*field, _ = cutUTF8(*field, smaller)
		*cut = true
	}
}

// PromptContextText joins the context of the prompt_submit commands into the text of the
// message the model reads after the input.
func PromptContextText(context []HookContext, maxOutputBytes int64) string {
	return HookContextText(HookEventPromptSubmit, context, maxOutputBytes)
}

// HookContextText joins the context commands of event into the text of one message.
func HookContextText(event string, context []HookContext, maxOutputBytes int64) string {
	var builder strings.Builder
	for index, item := range context {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString("[" + event + " hook " + item.Hook + "]\n")
		builder.WriteString(item.Text)
		if item.Cut {
			fmt.Fprintf(&builder, "\n[output cut to about %d bytes (hooks.max_output_bytes)]", maxOutputBytes)
		}
	}
	return builder.String()
}
