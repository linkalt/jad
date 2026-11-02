package app

import (
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/tools/native"
)

// compactUsageJSON is the usage every completed response of the fixture carries, so the
// target has a last completed response to read tokens from.
const compactUsageJSON = `"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6},`

// compactAfterTarget writes the target's transcript with the given last response input tokens
// (nil writes none, so the target has no completed response) and reports whether a message
// with the given gate settings compacts it.
func compactAfterTarget(t *testing.T, tokens *int, compact bool, compactAfter int64) bool {
	t.Helper()
	f := newLayoutFixture(t)
	if tokens != nil {
		messages := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "messages.jsonl")
		if err := os.MkdirAll(filepath.Dir(messages), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(messages, []byte(profileUsageTranscript(*tokens)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := countCompactions(f)
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", Compact: compact, CompactAfter: compactAfter}
	if _, err := (profileMessenger{resolver: f.service.resolver}).send(callerRunContext(t, f, 1), request); err != nil {
		t.Fatal(err)
	}
	return len(*calls) == 1
}

func TestProfileMessageCompactOffNeverCompactsWhateverCompactAfterIs(t *testing.T) {
	tokens := 10
	if compacted := compactAfterTarget(t, &tokens, false, 1); compacted {
		t.Fatal("compact off compacted the target with compact_after 1")
	}
}

func TestProfileMessageCompactOnWithDefaultCompactAfterCompactsOverZero(t *testing.T) {
	tokens := 5
	if !compactAfterTarget(t, &tokens, true, 0) {
		t.Fatal("compact on with compact_after 0 did not compact a target with 5 input tokens")
	}
}

func TestProfileMessageCompactAfterIsStrictlyGreater(t *testing.T) {
	tokens := 10
	if compactAfterTarget(t, &tokens, true, 10) {
		t.Fatal("compacted at tokens equal to compact_after; the threshold is strictly greater")
	}
	if !compactAfterTarget(t, &tokens, true, 9) {
		t.Fatal("did not compact at 10 tokens with compact_after 9")
	}
}

func TestProfileMessageCompactOnWithoutUsageDoesNotCompact(t *testing.T) {
	if compactAfterTarget(t, nil, true, 0) {
		t.Fatal("compacted a target with no completed response")
	}
}
