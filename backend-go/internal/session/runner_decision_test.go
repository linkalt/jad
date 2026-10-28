package session

import (
	"sync"
	"testing"

	"jad/backend-go/internal/tools"
)

func answerOf(decision tools.InteractionDecision, outcome tools.InteractionOutcome) tools.InteractionAnswer {
	return tools.InteractionAnswer{Decision: decision, Outcome: outcome}
}

func TestDecisionRecorderKeepsFirstNegativeAnswer(t *testing.T) {
	accept := answerOf(tools.InteractionAccept, tools.InteractionAnswered)
	for name, negative := range map[string]tools.InteractionAnswer{
		"decline":   answerOf(tools.InteractionDecline, tools.InteractionAnswered),
		"timeout":   answerOf(tools.InteractionDecline, tools.InteractionTimedOut),
		"cancelled": answerOf(tools.InteractionDecline, tools.InteractionCancelled),
	} {
		t.Run(name+" then accept", func(t *testing.T) {
			var rec decisionRecorder
			rec.record(negative)
			rec.record(accept)
			if !rec.declined() {
				t.Fatal("a later accept overwrote the negative answer")
			}
		})
		t.Run("accept then "+name, func(t *testing.T) {
			var rec decisionRecorder
			rec.record(accept)
			rec.record(negative)
			if !rec.declined() {
				t.Fatal("a later negative answer was lost")
			}
		})
	}
	var rec decisionRecorder
	if rec.declined() {
		t.Fatal("nothing recorded must not count as declined")
	}
	rec.record(accept)
	if rec.declined() {
		t.Fatal("an accept alone must not count as declined")
	}
}

func TestDecisionRecorderConcurrentWriters(t *testing.T) {
	var rec decisionRecorder
	var wg sync.WaitGroup
	for _, answer := range []tools.InteractionAnswer{
		answerOf(tools.InteractionAccept, tools.InteractionAnswered),
		answerOf(tools.InteractionDecline, tools.InteractionAnswered),
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec.record(answer)
		}()
	}
	wg.Wait()
	if !rec.declined() {
		t.Fatal("the decline was lost to a concurrent accept")
	}
}
