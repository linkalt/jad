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
	// MayHaveNewer is set when the last step shown may not be the child's newest response:
	// a response.completed record follows it that the snapshot does not show (a limit
	// ended the snapshot, the response was refused, or its calls still wait for a result),
	// or the reader could not give the builder every line (a line over its length limit,
	// a file over its scan limit). The browser then keeps that step's text, which the
	// result above may not hold. It errs towards true: a step's text shown twice costs a
	// line, a text shown nowhere costs the user a part of what the child said. It is a fact
	// about this read of the snapshot, so it belongs to the route body and is not part of
	// the Summary a tool result carries.
	MayHaveNewer bool `json:"mayHaveNewer,omitempty"`
	// EarlierOmitted is set when steps before the first one shown were left out: the
	// snapshot keeps the newest steps within its limits and drops the oldest whole steps, or
	// the reader began inside the file (a transcript over its scan window), or the records
	// before the first step could not be read. Absent when the first step shown is the
	// child's first. It never says how many; the dropped steps are not counted. Like
	// MayHaveNewer it belongs to the route body, not to the Summary a tool result carries.
	EarlierOmitted bool `json:"earlierOmitted,omitempty"`
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
//
// A result that handed the child off (it names the run's state) always has a summary, even
// before the child has completed a step: a still-running child has a transcript to open,
// which fills in as its steps complete. The count is then 0.
func SubagentSummaryOf(call SubagentCall, snapshot *SubagentSnapshot) *SubagentSummary {
	summary := snapshot.Summary()
	if summary == nil && call.HandedOff {
		summary = &SubagentSummary{Agent: call.Agent}
	}
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

// SubagentLimits bounds a snapshot. A snapshot keeps the newest steps: a step over the step
// or byte limit pushes the oldest whole steps out (EarlierOmitted), so a snapshot never
// grows with its child and never shows a step cut in half at its start. A part or result
// over its own limit is cut and marked with Truncated.
type SubagentLimits struct {
	// MaxSteps is the most steps a snapshot keeps; the newest ones win.
	MaxSteps int
	// MaxPartsPerStep is the most text, reasoning and tool_call parts one step keeps; the
	// result of a kept call comes on top, so a step holds at most twice as many parts.
	MaxPartsPerStep int
	// MaxPartBytes cuts the text, reasoning or arguments of one part.
	MaxPartBytes int
	// MaxResultBytes cuts the output of one tool result.
	MaxResultBytes int
	// MaxTotalBytes bounds the text of the whole snapshot. One step holds at most this alone
	// (its later parts are left out, a tool name aside), so the newest step is always shown,
	// and older steps stay only while the sum fits.
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
	// HandedOff is set when the result names the run's state, so the call returned while the
	// child was still queued or running. Its summary exists before the child's first step.
	HandedOff bool
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
	return SubagentCall{CallID: call.CallID, Agent: agent, Transcript: information.transcript, OmitsLastText: omits, HandedOff: information.state != ""}, true
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
//
// The snapshot is a window over the newest steps: a step that joins it may push the oldest
// ones out, whole, when MaxSteps or MaxTotalBytes is exceeded. The builder therefore reads
// every line it is given and never asks the reader to stop.
type SubagentBuilder struct {
	limits   SubagentLimits
	snapshot SubagentSnapshot
	// total is the bytes of the steps the snapshot holds; sizes is that of each of them.
	total int
	sizes []int
	// taken is set once a response became a step (or the pending one); omitted says steps
	// before the first one shown were left out (SubagentSnapshot.EarlierOmitted).
	taken   bool
	omitted bool
	// pending is the step whose response is written but whose calls still wait for a
	// result; it joins the snapshot when the last result arrives, or when the child moves
	// on without one. A child that is still running a tool leaves it unreported.
	pending *pendingStep
	// newer is set when a response.completed record (or a line the reader skipped) came
	// after the last step the snapshot holds and is not part of it; a response taken as a
	// step later clears it, since that one is newer (a skipped line after it sets it again). Once a limit has ended the snapshot it is the
	// only thing the lines left still decide.
	newer bool
}

type pendingStep struct {
	step SubagentStep
	// bytes is what the step holds against MaxTotalBytes.
	bytes int
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

// Add takes one complete line of the child's transcript and reports whether more lines can
// still change the snapshot. It always does: a later step is newer than every step held,
// so the window moves on, and the reader's own bound on its input is what ends the scan.
// The cost is a parse of every line that names a response or a result.
func (b *SubagentBuilder) Add(offset int64, line []byte) bool {
	// The markers only decide whether a line is worth parsing; the record's own type decides
	// what it is, since a tool output may well contain the words of a marker.
	if !bytes.Contains(line, completedMarker) && !bytes.Contains(line, outputMarker) && !bytes.Contains(line, messageMarker) {
		return true
	}
	var head subagentHead
	if json.Unmarshal(line, &head) != nil {
		// Not a record the builder can read; one that names a response may still be one.
		if bytes.Contains(line, completedMarker) {
			b.newer = true
		}
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
	return true
}

// Unseen tells the builder that the reader did not give it everything after the lines it
// has had: a line over the reader's length limit was skipped. It may hold a response newer
// than the last step, so the snapshot says so (MayHaveNewer) unless a response that follows
// becomes a step.
func (b *SubagentBuilder) Unseen() {
	b.newer = true
}

// EarlierUnread tells the builder that the reader began inside the child's transcript, so
// the lines before the first one it gave were never seen. Steps may precede the first one
// the snapshot shows (SubagentSnapshot.EarlierOmitted).
func (b *SubagentBuilder) EarlierUnread() {
	b.omitted = true
}

// addResponse starts the step of a response.completed record. A record it refuses (a
// limit is reached, the provider did not complete it, it cannot be read or has nothing
// to keep) is still a response the child wrote after the last step, so it sets newer.
func (b *SubagentBuilder) addResponse(offset int64, line []byte, head subagentHead) {
	// A response the provider reports as anything but completed is not a completed step.
	if head.Response.Status != "" && head.Response.Status != "completed" {
		b.newer = true
		return
	}
	messages, err := NormalizeTranscript([]TranscriptRecord{{Offset: offset, Prefix: "o", Raw: line}})
	if err != nil || len(messages) == 0 {
		b.newer = true
		return
	}
	parts, size := b.keep(messages[0].Parts)
	if len(parts) == 0 {
		b.newer = true
		return
	}
	id := strings.TrimPrefix(messages[0].ID, "session-assistant-")
	if head.SessionTurn != nil && head.SessionStep != nil {
		id = strconv.FormatInt(*head.SessionTurn, 10) + "." + strconv.FormatInt(*head.SessionStep, 10)
	}
	pending := &pendingStep{step: SubagentStep{ID: id, Status: SubagentStepCompleted, Parts: parts}, bytes: size, open: map[string]bool{}}
	for _, part := range parts {
		if part.Type == "tool_call" {
			pending.open[part.CallID] = true
		}
	}
	b.pending = pending
	// What was refused or skipped before the first response taken precedes every step shown.
	if b.newer && !b.taken {
		b.omitted = true
	}
	b.taken = true
	// This response is the newest now; what is refused or skipped from here on is newer still.
	b.newer = false
	// A step with no call has nothing to wait for.
	if len(pending.open) == 0 {
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
	// The step's own budget is spent: its remaining calls get no result.
	remaining := b.limits.MaxTotalBytes - pending.bytes
	if remaining <= 0 {
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
	pending.bytes += len(output)
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
	b.sizes = append(b.sizes, pending.bytes)
	b.total += pending.bytes
	// The oldest whole steps make room; the step just added stays (it alone fits the byte
	// limit, since a step is built within it).
	for len(b.snapshot.Steps) > b.limits.MaxSteps || (b.total > b.limits.MaxTotalBytes && len(b.snapshot.Steps) > 1) {
		b.total -= b.sizes[0]
		b.snapshot.Steps[0] = SubagentStep{}
		b.snapshot.Steps = b.snapshot.Steps[1:]
		b.sizes = b.sizes[1:]
		b.omitted = true
	}
}

// Snapshot is what the lines added so far completed, nil when no step was and nothing is
// known to be missing. A step whose calls still wait for a result at the end of the lines
// is not completed and is left out; that withheld step is a newer response, so the
// snapshot says it may have one.
//
// With no completed step but a known gap (the reader began inside the transcript, a line
// was refused, a step waits for its result), the snapshot has no steps and says so with
// its flags: an empty snapshot without them would read as a complete, empty transcript.
// Its Summary is still nil.
func (b *SubagentBuilder) Snapshot() *SubagentSnapshot {
	if len(b.snapshot.Steps) == 0 && !b.omitted && !b.newer && b.pending == nil {
		return nil
	}
	snapshot := b.snapshot
	if snapshot.Steps == nil {
		snapshot.Steps = []SubagentStep{}
	}
	snapshot.MayHaveNewer = b.newer || b.pending != nil
	snapshot.EarlierOmitted = b.omitted
	return &snapshot
}

// keep copies the renderable parts of one response within the limits: text, reasoning and
// tool_call parts only; the results are added as their records arrive.
func (b *SubagentBuilder) keep(parts []Part) ([]Part, int) {
	kept := make([]Part, 0, min(len(parts), b.limits.MaxPartsPerStep))
	used := 0
	for _, part := range parts {
		if len(kept) >= b.limits.MaxPartsPerStep {
			break
		}
		remaining := b.limits.MaxTotalBytes - used
		if remaining <= 0 {
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
			used += len(text)
			kept = append(kept, next)
		case "tool_call":
			if part.Arguments == nil || part.CallID == "" || part.Name == "" {
				continue
			}
			// The name counts against the step's budget too (the arguments give way to it).
			arguments := cutUTF8(*part.Arguments, min(limit, max(remaining-len(part.Name), 0)))
			next := Part{Type: "tool_call", CallID: part.CallID, Name: part.Name, Arguments: &arguments}
			if len(arguments) < len(*part.Arguments) {
				next.Truncated = &PartTruncation{Bytes: len(*part.Arguments)}
			}
			used += len(arguments) + len(part.Name)
			kept = append(kept, next)
		}
	}
	return kept, used
}
