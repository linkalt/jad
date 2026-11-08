package webui

import (
	"bytes"
	"cmp"
	"encoding/json"
	"strconv"
	"strings"
)

// SubagentStepCompleted is the only status a subagent step is ever given: a step exists
// only because its response.completed record was written, and nothing running or queued
// is ever shown. The browser ignores any other status.
const SubagentStepCompleted = "completed"

// SubagentSnapshot is a one-shot, bounded copy of the steps a subagent run had completed
// when a tool result was served. It is built from the child's transcript, never streamed,
// and is never changed after it is built: parts of different messages and caches share
// one snapshot, so a holder must copy before it edits.
type SubagentSnapshot struct {
	Agent string         `json:"agent,omitempty"`
	Steps []SubagentStep `json:"steps"`
}

// SubagentSummary is what a tool result carries of its child: the agent and how many
// completed steps the child had when the result was served. It holds no step, so the
// payload of a transcript does not grow with its children; the steps are read on demand
// from the subagent route (a SubagentSnapshot). A holder never edits it after it is set.
type SubagentSummary struct {
	Agent string `json:"agent,omitempty"`
	Steps int    `json:"steps"`
	// OmitsLastText is set when the result does not hold, whole, the text of the last step the
	// child completed (SubagentCall.OmitsLastText), so the browser shows that text instead of
	// leaving it out as the copy the result already shows. Absent (false) for a result that
	// came back with that text uncut, and from a server that predates it.
	OmitsLastText bool `json:"omitsLastText,omitempty"`
}

// Summary is the descriptor of a snapshot, nil for a missing or empty one, so a result
// without a child to show stays as it is.
func (s *SubagentSnapshot) Summary() *SubagentSummary {
	if s == nil || len(s.Steps) == 0 {
		return nil
	}
	return &SubagentSummary{Agent: s.Agent, Steps: len(s.Steps)}
}

// SubagentSummaryOf is what the tool result of call carries of its child: the summary of
// snapshot, marked when the result omits the child's last text. Every path that sets a
// result's summary (the live run and the replay) builds it here, so they cannot differ.
func SubagentSummaryOf(call SubagentCall, snapshot *SubagentSnapshot) *SubagentSummary {
	summary := snapshot.Summary()
	if summary != nil {
		summary.OmitsLastText = call.OmitsLastText
	}
	return summary
}

// SubagentStep is one completed step of the child, as the parts of a message step: text,
// reasoning, tool_call and the tool_result of each kept call, in the shapes the
// conversation uses, so a call and its result pair by call id. A step is completed when its
// response.completed record is written and every call it kept has its result, or when the
// child has moved on past it. Hosted-tool and nested subagent parts are never kept.
type SubagentStep struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Parts  []Part `json:"parts"`
}

// SubagentLimits bounds a snapshot. A step, part or byte over a limit is left out (text
// is cut and marked with Truncated), so a snapshot never grows with its child.
type SubagentLimits struct {
	// MaxSteps is the most steps a snapshot keeps; the first ones win.
	MaxSteps int
	// MaxPartsPerStep is the most text, reasoning and tool_call parts one step keeps; the
	// result of a kept call comes on top, so a step holds at most twice as many parts.
	MaxPartsPerStep int
	// MaxPartBytes cuts the text, reasoning or arguments of one part.
	MaxPartBytes int
	// MaxResultBytes cuts the output of one tool result.
	MaxResultBytes int
	// MaxTotalBytes bounds the text of the whole snapshot.
	MaxTotalBytes int
}

// DefaultSubagentLimits is the bound a served snapshot has unless a caller sets another.
func DefaultSubagentLimits() SubagentLimits {
	return SubagentLimits{MaxSteps: 32, MaxPartsPerStep: 12, MaxPartBytes: 2048, MaxResultBytes: 4096, MaxTotalBytes: 48 * 1024}
}

// SubagentCall identifies the child of one subagent tool result.
type SubagentCall struct {
	CallID string
	// Agent is the agent name the result or the call reports, empty when neither does.
	Agent string
	// Transcript is the child's transcript path as the result printed it, empty when it
	// printed none. It is only a hint: a reader must confine it before it opens it.
	Transcript string
	// OmitsLastText is set when the result does not hold, whole, the text of the last step
	// the child completed, so that text is shown rather than left out as the result's copy.
	// The native tool (renderRun) puts that text in the content of a finished, stopped or
	// timed-out result, which name no state and no error code but subagent_timeout; any
	// other result omits it: a handed-off one names the run's state and holds a run id, a
	// cut one names truncated, a failed or empty one holds no text, and a lost one holds the
	// newest text the child wrote, which can be that of a step whose tool results were never
	// written and which the snapshot therefore leaves out. It is not part of what identifies
	// the child, so a reader does not key on it.
	OmitsLastText bool
}

// IsSubagentTool reports whether a tool call name is the native subagent tool. It is the
// only discriminator: profile_message prints the same transcript line and carries no
// subagent steps.
func IsSubagentTool(name string) bool {
	return name == "subagent" || name == "native.subagent"
}

// SubagentCallOf describes the child of a tool result when its tool call is the subagent
// tool. The agent is read from the result's information first (it names the agent that
// ran), then from the call's arguments.
func SubagentCallOf(call, result Part) (SubagentCall, bool) {
	if call.Type != "tool_call" || result.Type != "tool_result" || !IsSubagentTool(call.Name) || call.CallID == "" || call.CallID != result.CallID {
		return SubagentCall{}, false
	}
	information := subagentInformation(result.Output)
	agent := information.agent
	if agent == "" && call.Arguments != nil {
		var arguments struct {
			Agent string `json:"agent"`
		}
		if json.Unmarshal([]byte(*call.Arguments), &arguments) == nil {
			agent = strings.TrimSpace(arguments.Agent)
		}
	}
	omits := information.state != "" || information.truncated || (information.errorCode != "" && information.errorCode != "subagent_timeout")
	return SubagentCall{CallID: call.CallID, Agent: agent, Transcript: information.transcript, OmitsLastText: omits}, true
}

// subagentResultInformation is what the reader takes from a subagent result's information.
type subagentResultInformation struct {
	agent, transcript, state, errorCode string
	truncated                           bool
}

// subagentInformation reads the agent, transcript, state, error_code and truncated lines of
// a tool result's INFORMATION section: "key: value" lines after the marker, up to the first
// blank line. The first non-empty value of a key counts.
func subagentInformation(output string) subagentResultInformation {
	var information subagentResultInformation
	_, section, found := strings.Cut(output, "\n--- INFORMATION ---\n")
	if !found {
		return information
	}
	for len(section) > 0 {
		var line string
		line, section, _ = strings.Cut(section, "\n")
		if line == "" {
			break
		}
		key, value, _ := strings.Cut(line, ": ")
		value = strings.TrimSpace(value)
		switch key {
		case "agent":
			information.agent = cmp.Or(information.agent, value)
		case "transcript":
			information.transcript = cmp.Or(information.transcript, value)
		case "state":
			information.state = cmp.Or(information.state, value)
		case "error_code":
			information.errorCode = cmp.Or(information.errorCode, value)
		case "truncated":
			information.truncated = information.truncated || value == "true"
		}
	}
	return information
}

// AttachSubagents sets Subagent, the summary of the snapshot lookup answers, on the tool
// results of subagent calls (nil leaves the part as it is). It never edits messages or parts it was given: a
// message that gains a snapshot is copied. Call it only where a snapshot is served; it
// reads nothing but what lookup reads.
func AttachSubagents(messages []Message, lookup func(SubagentCall) *SubagentSnapshot) []Message {
	if lookup == nil {
		return messages
	}
	result := messages
	copied := false
	for index := range messages {
		message := messages[index]
		var calls map[string]Part
		for _, part := range message.Parts {
			if part.Type == "tool_call" && IsSubagentTool(part.Name) && part.CallID != "" {
				if calls == nil {
					calls = map[string]Part{}
				}
				calls[part.CallID] = part
			}
		}
		if calls == nil {
			continue
		}
		var parts []Part
		for partIndex, part := range message.Parts {
			if part.Type != "tool_result" || part.Subagent != nil {
				continue
			}
			call, ok := SubagentCallOf(calls[part.CallID], part)
			if !ok {
				continue
			}
			summary := SubagentSummaryOf(call, lookup(call))
			if summary == nil {
				continue
			}
			if parts == nil {
				parts = append([]Part(nil), message.Parts...)
			}
			parts[partIndex].Subagent = summary
		}
		if parts == nil {
			continue
		}
		if !copied {
			result = append([]Message(nil), messages...)
			copied = true
		}
		message.Parts = parts
		result[index] = message
	}
	return result
}

var (
	completedMarker = []byte("response.completed")
	outputMarker    = []byte("function_call_output")
	messageMarker   = []byte(`"message"`)
)

// SubagentBuilder builds a snapshot from the lines of a child's transcript, in file order.
// The child writes its transcript with the writer a session uses: a response.completed
// line, then one function_call_output line per call it made. Each record is normalized by
// NormalizeTranscript, so the parts are the conversation's own, and the grouping is the
// child's: one response.completed line is one step, and the results that follow it join it.
type SubagentBuilder struct {
	limits   SubagentLimits
	snapshot SubagentSnapshot
	total    int
	done     bool
	// pending is the step whose response is written but whose calls still wait for a
	// result; it joins the snapshot when the last result arrives, or when the child moves
	// on without one. A child that is still running a tool leaves it unreported.
	pending *pendingStep
}

type pendingStep struct {
	step SubagentStep
	// open holds the call ids of the step that have no result yet.
	open map[string]bool
}

// NewSubagentBuilder starts a snapshot for agent. A limit of zero or less takes its default.
func NewSubagentBuilder(agent string, limits SubagentLimits) *SubagentBuilder {
	defaults := DefaultSubagentLimits()
	if limits.MaxSteps <= 0 {
		limits.MaxSteps = defaults.MaxSteps
	}
	if limits.MaxPartsPerStep <= 0 {
		limits.MaxPartsPerStep = defaults.MaxPartsPerStep
	}
	if limits.MaxPartBytes <= 0 {
		limits.MaxPartBytes = defaults.MaxPartBytes
	}
	if limits.MaxResultBytes <= 0 {
		limits.MaxResultBytes = defaults.MaxResultBytes
	}
	if limits.MaxTotalBytes <= 0 {
		limits.MaxTotalBytes = defaults.MaxTotalBytes
	}
	return &SubagentBuilder{limits: limits, snapshot: SubagentSnapshot{Agent: agent}}
}

// subagentHead is the part of a transcript record the builder reads before it normalizes it.
type subagentHead struct {
	Type        string `json:"type"`
	Role        string `json:"role"`
	CallID      string `json:"call_id"`
	SessionTurn *int64 `json:"session_turn"`
	SessionStep *int64 `json:"session_step"`
	Response    struct {
		Status string `json:"status"`
	} `json:"response"`
}

// Add takes one complete line of the child's transcript and reports whether more lines
// can still change the snapshot: false once a limit is reached.
func (b *SubagentBuilder) Add(offset int64, line []byte) bool {
	if b.done {
		return false
	}
	// The markers only decide whether a line is worth parsing; the record's own type decides
	// what it is, since a tool output may well contain the words of a marker.
	if !bytes.Contains(line, completedMarker) && !bytes.Contains(line, outputMarker) && !bytes.Contains(line, messageMarker) {
		return true
	}
	var head subagentHead
	if json.Unmarshal(line, &head) != nil {
		return true
	}
	switch {
	case head.Type == "response.completed":
		// The child has moved on, so a step still waiting for a result will not get one.
		b.closePending()
		b.addResponse(offset, line, head)
	case head.Type == "function_call_output":
		b.addResult(offset, line, head)
	case head.Type == "message" && head.Role == "user":
		b.closePending()
	}
	return !b.done
}

func (b *SubagentBuilder) addResponse(offset int64, line []byte, head subagentHead) {
	if b.done {
		return
	}
	// A response the provider reports as anything but completed is not a completed step.
	if head.Response.Status != "" && head.Response.Status != "completed" {
		return
	}
	messages, err := NormalizeTranscript([]TranscriptRecord{{Offset: offset, Prefix: "o", Raw: line}})
	if err != nil || len(messages) == 0 {
		return
	}
	parts := b.keep(messages[0].Parts)
	if len(parts) == 0 {
		return
	}
	id := strings.TrimPrefix(messages[0].ID, "session-assistant-")
	if head.SessionTurn != nil && head.SessionStep != nil {
		id = strconv.FormatInt(*head.SessionTurn, 10) + "." + strconv.FormatInt(*head.SessionStep, 10)
	}
	pending := &pendingStep{step: SubagentStep{ID: id, Status: SubagentStepCompleted, Parts: parts}, open: map[string]bool{}}
	for _, part := range parts {
		if part.Type == "tool_call" {
			pending.open[part.CallID] = true
		}
	}
	b.pending = pending
	// A step with no call has nothing to wait for; so does one the byte budget cut short.
	if len(pending.open) == 0 || b.done {
		b.closePending()
	}
}

// addResult pairs a function_call_output record with its call in the pending step. A
// result of a call that was not kept, or that has been paired, is ignored.
func (b *SubagentBuilder) addResult(offset int64, line []byte, head subagentHead) {
	pending := b.pending
	if pending == nil || !pending.open[head.CallID] {
		return
	}
	remaining := b.limits.MaxTotalBytes - b.total
	if remaining <= 0 {
		b.done = true
		b.closePending()
		return
	}
	messages, err := NormalizeTranscript([]TranscriptRecord{{Offset: offset, Prefix: "o", Raw: line}})
	if err != nil || len(messages) != 1 || len(messages[0].Parts) != 1 || messages[0].Parts[0].Type != "tool_result" {
		return
	}
	read := messages[0].Parts[0]
	limit := min(b.limits.MaxResultBytes, remaining)
	output := cutUTF8(read.Output, limit)
	// Images stay with the child's transcript: the browser fetches them by the parent's
	// session, so they would not resolve there.
	result := Part{Type: "tool_result", CallID: read.CallID, Output: output, Status: read.Status}
	if len(output) < len(read.Output) {
		result.Truncated = &PartTruncation{Bytes: len(read.Output)}
	}
	b.total += len(output)
	pending.step.Parts = append(pending.step.Parts, result)
	delete(pending.open, head.CallID)
	if len(pending.open) == 0 {
		b.closePending()
	}
}

// closePending adds the pending step to the snapshot, with the results it has.
func (b *SubagentBuilder) closePending() {
	pending := b.pending
	b.pending = nil
	if pending == nil {
		return
	}
	b.snapshot.Steps = append(b.snapshot.Steps, pending.step)
	if len(b.snapshot.Steps) >= b.limits.MaxSteps {
		b.done = true
	}
}

// Snapshot is what the lines added so far completed, nil when no step was. A step whose
// calls still wait for a result at the end of the lines is not completed and is left out.
func (b *SubagentBuilder) Snapshot() *SubagentSnapshot {
	if len(b.snapshot.Steps) == 0 {
		return nil
	}
	snapshot := b.snapshot
	return &snapshot
}

// keep copies the renderable parts of one response within the limits: text, reasoning and
// tool_call parts only; the results are added as their records arrive.
func (b *SubagentBuilder) keep(parts []Part) []Part {
	kept := make([]Part, 0, min(len(parts), b.limits.MaxPartsPerStep))
	for _, part := range parts {
		if len(kept) >= b.limits.MaxPartsPerStep {
			break
		}
		remaining := b.limits.MaxTotalBytes - b.total
		if remaining <= 0 {
			b.done = true
			break
		}
		limit := min(b.limits.MaxPartBytes, remaining)
		switch part.Type {
		case "text", "reasoning":
			if part.Text == "" {
				continue
			}
			text := cutUTF8(part.Text, limit)
			next := Part{Type: part.Type, Text: text}
			if len(text) < len(part.Text) {
				next.Truncated = &PartTruncation{Bytes: len(part.Text)}
			}
			b.total += len(text)
			kept = append(kept, next)
		case "tool_call":
			if part.Arguments == nil || part.CallID == "" || part.Name == "" {
				continue
			}
			arguments := cutUTF8(*part.Arguments, limit)
			next := Part{Type: "tool_call", CallID: part.CallID, Name: part.Name, Arguments: &arguments}
			if len(arguments) < len(*part.Arguments) {
				next.Truncated = &PartTruncation{Bytes: len(*part.Arguments)}
			}
			b.total += len(arguments) + len(part.Name)
			kept = append(kept, next)
		}
	}
	return kept
}
