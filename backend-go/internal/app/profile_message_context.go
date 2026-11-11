package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

// contextStep returns the BeforeRun step of a profile_message to plan.target for the
// configured context trigger, or nil when the trigger is off. The step runs with the target's
// transcript held and the run registered, so a compaction or reset cannot meet another writer.
//
// The trigger is evaluated on every message that starts a run of the target (a queued message
// when the pump starts it, a sub-agent's message too) and keeps no state between messages: a
// target that was empty, below After, or whose compaction or reset failed at one message is
// looked at again at the next, and one that grew past After again is acted on again. That
// holds for a reset as for a compact, so a reset is destructive at every qualifying message: it
// drops what the messages before it wrote. Nothing depends on the caller's run (a call without
// one, or from a run that has ended, is evaluated like any other).
//
// A target whose messages.jsonl is missing, empty or blank is left alone, with the Warn
// "messages.jsonl is empty - nothing to compact|reset" for either action (a compaction can leave
// only the active tail, so a reset does not clear a compaction's summary until the target has
// answered again). With After zero a target with content is acted on without counting tokens.
// Above zero the tokens are those of the last completed response in messages.jsonl itself, never
// usage-cache.json, which a compaction leaves with zeroed counts; the action runs once they pass
// After.
//
// A failed compaction is logged and the message is sent anyway. A reset that is due and cannot
// be carried out fails closed: the step returns a *native.ContextTriggerError and the message is
// not sent, so the target cannot answer from the history it was asked to drop. That covers a
// reset that failed and a transcript or token count that could not be read. The next message
// tries again.
func (m profileMessenger) contextStep(plan *profileMessagePlan, trigger sessionfs.ProfileMessageContextTrigger) func(context.Context) error {
	if trigger.Action != sessionfs.ProfileMessageContextCompact && trigger.Action != sessionfs.ProfileMessageContextReset {
		return nil
	}
	reset := trigger.Action == sessionfs.ProfileMessageContextReset
	verb, noun := "compacting", "compact"
	if reset {
		verb, noun = "resetting", "reset"
	}
	return func(ctx context.Context) error {
		logger := m.resolver.logger
		// fail ends the step on a problem. A reset fails the message; a compaction only logs.
		fail := func(reason, what string, err error) error {
			if !reset {
				logger.Warn("profile_message "+what+"; not "+verb, "session", plan.reference, "profile", plan.target, "error", err)
				return nil
			}
			logger.Warn("profile_message "+what+"; the reset is not done and the message is not sent", "session", plan.reference, "profile", plan.target, "error", err)
			return &native.ContextTriggerError{Action: trigger.Action, Reason: reason, Err: err}
		}
		messages := plan.targetMessages
		if messages == "" {
			config, err := m.profileConfig(plan.reference, plan.target)
			if err != nil {
				return fail("the target's transcript could not be located", "could not locate the target transcript", err)
			}
			messages = config.MessagesPath
		}
		filled, err := transcriptHasContent(messages)
		if err != nil {
			return fail("the target's transcript could not be read", "could not read the target transcript", err)
		}
		if !filled {
			logger.Warn("messages.jsonl is empty - nothing to "+noun, "session", plan.reference, "profile", plan.target)
			return nil
		}
		if trigger.After > 0 {
			profileTokens := m.resolver.profileUsage
			if profileTokens == nil {
				profileTokens = m.transcriptTokens
			}
			tokens, err := profileTokens(ctx, plan.reference, plan.target)
			if err != nil {
				return fail("the target's tokens could not be counted", "usage read failed", err)
			}
			if tokens == nil || *tokens <= trigger.After {
				return nil
			}
		}
		act := m.resolver.compactProfile
		if reset {
			act = m.resolver.resetProfile
		}
		if act == nil {
			act = m.resolver.compactTarget
			if reset {
				act = m.resolver.resetTarget
			}
		}
		if err := act(ctx, plan.reference, plan.target); err != nil {
			if !reset {
				logger.Warn("profile_message "+noun+" failed; sending the message anyway", "session", plan.reference, "profile", plan.target, "error", err)
				return nil
			}
			var published *transcript.PublishedError
			if errors.As(err, &published) {
				// The history is cleared and the children are stopped, but a crash could undo it.
				return fail("the reset was applied but could not be confirmed durable and may need attention", "reset applied but not confirmed durable", err)
			}
			return fail("the reset failed and may need attention", "reset failed", err)
		}
		return nil
	}
}

// transcriptTokens is the input tokens of the last completed response in the target's
// messages.jsonl, or nil when it holds none. Unlike ProfileInfo it reads the transcript alone:
// usage-cache.json is neither read nor written, so a stale cache (or the zeroed counts a
// compaction leaves in it) cannot decide the trigger.
func (m profileMessenger) transcriptTokens(ctx context.Context, session, profile string) (*int64, error) {
	config, err := m.profileConfig(session, profile)
	if err != nil {
		return nil, err
	}
	shared, release, err := m.resolver.sharedStore(config)
	if err != nil {
		return nil, err
	}
	defer release()
	if shared == nil || shared.store == nil {
		return nil, errors.New("shared transcript store is not initialized")
	}
	var summary webui.UsageSummary
	scan := func() error {
		summary = webui.UsageSummary{}
		seen := map[string]struct{}{}
		return scanSnapshotRead(shared.store, 0, 1<<63-1, func(offset int64, line []byte) error {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
				return fmt.Errorf("transcript record at byte %d is not a JSON object", offset)
			}
			var recordType string
			_ = json.Unmarshal(fields["type"], &recordType)
			if recordType != "response.completed" {
				return nil
			}
			var response map[string]json.RawMessage
			if json.Unmarshal(fields["response"], &response) != nil {
				response = nil
			}
			if err := webui.AddCompletedRecord(&summary, line, fields, response, seen); err != nil {
				return fmt.Errorf("read usage at byte %d: %w", offset, err)
			}
			return nil
		})
	}
	// A snapshot read that raced a rewrite is discarded and retried once, as the usage scan does.
	err = scan()
	if errors.Is(err, transcript.ErrTranscriptChanged) {
		err = scan()
	}
	if err != nil {
		return nil, fmt.Errorf("read usage of profile %q: %w", profile, err)
	}
	if summary.Current == nil || summary.Current.InputTokens == nil {
		return nil, nil
	}
	tokens := *summary.Current.InputTokens
	return &tokens, nil
}

// transcriptHasContent reports whether the file holds anything but whitespace. A missing file
// has none.
func transcriptHasContent(path string) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	buffer := make([]byte, 4096)
	for {
		n, err := file.Read(buffer)
		for _, b := range buffer[:n] {
			if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
				return true, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// resetTarget clears a profile_message target's history and what derives from it, then stops
// the session's children, as ResetProfile does. The caller holds the target's transcript lock
// and its run is registered, which ResetProfile would wait on or refuse, so this takes neither
// the mutation gate nor the process lock (compactTarget does the same): the held lock and the
// registered run already keep user sends, mutations and other writers out.
func (r *Resolver) resetTarget(ctx context.Context, reference, profile string) error {
	resolved, err := r.ResolveMaintenance(ctx, reference, profile)
	if err != nil {
		return err
	}
	if err := maintenanceReady(resolved); err != nil {
		return err
	}
	return r.resetResolved(ctx, resolved)
}
