package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fcItem(id, callID, arguments string) string {
	return fmt.Sprintf(`{"type":"function_call","id":%q,"call_id":%q,"name":"exec","arguments":%q,"status":"completed"}`, id, callID, arguments)
}

func messageItem(id, text string) string {
	idField := ""
	if id != "" {
		idField = fmt.Sprintf(`"id":%q,`, id)
	}
	return fmt.Sprintf(`{"type":"message",%s"role":"assistant","content":[{"type":"output_text","text":%q}]}`, idField, text)
}

func completedWith(items ...string) []byte {
	return []byte(`{"type":"response.completed","response":{"id":"resp_r","status":"completed","output":[` + strings.Join(items, ",") + `]}}`)
}

type stagedItem struct {
	index int64
	item  string
}

// commitStaged stages the items in the given arrival order and commits the completed
// event. It returns the committed output (nil when the commit failed), the error, the
// transcript bytes and the staged turn.
func commitStaged(t *testing.T, mode DurabilityMode, staged []stagedItem, completed []byte) (outputIDs []string, transcriptBytes string, turn *ResponseTurn, err error) {
	t.Helper()
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, mode, ResponseMetadataMinimal)
	turn, beginErr := store.BeginTurn("turn-reconcile")
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	for _, one := range staged {
		if err := turn.StageOutputItem(outputItemEvent(one.index, one.item)); err != nil {
			t.Fatal(err)
		}
	}
	_, err = turn.Commit(completed)
	data, _ := os.ReadFile(journal.path)
	transcriptBytes = string(data)
	if err == nil {
		fields, parseErr := objectFields([]byte(strings.TrimSpace(transcriptBytes)))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		response, _ := fieldValue(fields, "response")
		responseFields, _ := objectFields(response)
		output, _ := fieldValue(responseFields, "output")
		if arrayErr := arrayValues(output, func(item []byte) error {
			identity, identityErr := identityOfItem(item)
			if identityErr != nil {
				return identityErr
			}
			key := identity.id
			if key == "" {
				key = identity.callID
			}
			if key == "" {
				key = string(item)
			}
			outputIDs = append(outputIDs, key)
			return nil
		}); arrayErr != nil {
			t.Fatal(arrayErr)
		}
	}
	return outputIDs, transcriptBytes, turn, err
}

func TestCommitFollowsCompletedOutputWhenWireIndexesHaveGapsOrAreReordered(t *testing.T) {
	a, b, c := fcItem("fc_a", "call_a", `{"n":1}`), fcItem("fc_b", "call_b", `{"n":2}`), fcItem("fc_c", "call_c", `{"n":3}`)
	reasoning := `{"type":"reasoning","id":"rs_1","summary":[]}`
	cases := []struct {
		name      string
		staged    []stagedItem
		completed []string
		want      []string
	}{
		{"gap at 0: the only item has index 1", []stagedItem{{1, a}}, []string{a}, []string{"fc_a"}},
		{"gap at 0 and 1: the only item has index 2", []stagedItem{{2, a}}, []string{a}, []string{"fc_a"}},
		{"a reasoning item without a done event leaves the calls at 1 and 2", []stagedItem{{1, a}, {2, b}}, []string{a, b}, []string{"fc_a", "fc_b"}},
		{"reordered done events and indexes: output order wins", []stagedItem{{0, a}, {1, b}, {2, c}}, []string{c, a, b}, []string{"fc_c", "fc_a", "fc_b"}},
		{"reordered with gaps and arrival order unlike both", []stagedItem{{7, c}, {2, a}, {5, b}}, []string{b, c, a}, []string{"fc_b", "fc_c", "fc_a"}},
		{"mixed item types with an offset", []stagedItem{{4, messageItem("msg_1", "hi")}, {3, reasoning}, {5, a}}, []string{reasoning, messageItem("msg_1", "hi"), a}, []string{"rs_1", "msg_1", "fc_a"}},
		{"dense in order, the common case", []stagedItem{{0, a}, {1, b}}, []string{a, b}, []string{"fc_a", "fc_b"}},
		{"calls matched by call_id when the done events carry no id", []stagedItem{{3, `{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"}`}, {1, `{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"}`}},
			[]string{`{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"}`, `{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"}`}, []string{"call_b", "call_a"}},
		{"no ids anywhere: items with the same content pair across a gap", []stagedItem{{2, messageItem("", "x")}, {3, messageItem("", "y")}}, []string{messageItem("", "x"), messageItem("", "y")}, []string{messageItem("", "x"), messageItem("", "y")}},
	}
	for _, mode := range []DurabilityMode{DurabilityProcess, DurabilityStrict} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%v/%s", mode, tc.name), func(t *testing.T) {
				ids, written, turn, err := commitStaged(t, mode, tc.staged, completedWith(tc.completed...))
				if err != nil {
					t.Fatalf("Commit() error = %v", err)
				}
				// Canonical JSON sorts nothing in these fixtures, so identity-less items
				// compare as the strings given.
				if len(ids) != len(tc.want) {
					t.Fatalf("committed %d items, want %d: %v", len(ids), len(tc.want), ids)
				}
				for position, want := range tc.want {
					got := ids[position]
					if strings.HasPrefix(want, "{") {
						if !strings.Contains(got, `"text":"`+extractText(want)+`"`) {
							t.Fatalf("item %d = %s, want text of %s", position, got, want)
						}
						continue
					}
					if got != want {
						t.Fatalf("item %d = %q, want %q (output %v)", position, got, want, ids)
					}
				}
				if strings.Count(written, "\n") != 1 {
					t.Fatalf("transcript has %d lines, want 1", strings.Count(written, "\n"))
				}
				if turn.manifest.Status != "committed" {
					t.Fatalf("status = %q", turn.manifest.Status)
				}
			})
		}
	}
}

func extractText(item string) string {
	const marker = `"text":"`
	start := strings.Index(item, marker)
	if start < 0 {
		return ""
	}
	rest := item[start+len(marker):]
	return rest[:strings.Index(rest, `"`)]
}

func TestCommittedItemsAreTheStagedItemsPairedByIdNotTheCompletedCopies(t *testing.T) {
	// The staged copy is what the transcript keeps; the id picks which one.
	a := fcItem("fc_a", "call_a", `{"n":1}`)
	b := fcItem("fc_b", "call_b", `{"n":2}`)
	_, written, _, err := commitStaged(t, DurabilityProcess, []stagedItem{{1, a}, {3, b}}, completedWith(b, a))
	if err != nil {
		t.Fatal(err)
	}
	first, second := strings.Index(written, `"fc_b"`), strings.Index(written, `"fc_a"`)
	if first < 0 || second < 0 || first > second {
		t.Fatalf("committed order is not the completed order: %s", written)
	}
	if strings.Count(written, `"call_id":"call_a"`) != 1 || strings.Count(written, `"call_id":"call_b"`) != 1 {
		t.Fatalf("a call was lost or duplicated: %s", written)
	}
}

func TestCommitRefusesAnAmbiguousOrInconsistentMapping(t *testing.T) {
	a, b := fcItem("fc_a", "call_a", `{"n":1}`), fcItem("fc_b", "call_b", `{"n":2}`)
	sameIDOtherArguments := fcItem("fc_a", "call_a", `{"n":9}`)
	cases := []struct {
		name      string
		staged    []stagedItem
		completed []string
		want      string
	}{
		{"completed id never staged", []stagedItem{{1, a}}, []string{b}, "has no staged item"},
		{"two staged items share an id", []stagedItem{{1, a}, {2, sameIDOtherArguments}}, []string{a, sameIDOtherArguments}, "ambiguous"},
		{"two completed items share an id", []stagedItem{{1, a}, {2, b}}, []string{a, a}, "both match staged output_index"},
		{"same id, different arguments (dense)", []stagedItem{{0, a}}, []string{sameIDOtherArguments}, "differs from staged output_index 0 in arguments"},
		{"same id, different arguments (gap)", []stagedItem{{2, a}}, []string{sameIDOtherArguments}, "differs from staged output_index 2 in arguments"},
		{"no ids, gap, different content", []stagedItem{{2, messageItem("", "x")}}, []string{messageItem("", "y")}, "cannot be matched to staged output_index 2"},
		{"different types across a gap", []stagedItem{{2, a}}, []string{messageItem("msg_1", "x")}, "has no staged item"},
		{"count mismatch", []stagedItem{{1, a}}, []string{a, b}, "does not match completed output count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, written, turn, err := commitStaged(t, DurabilityProcess, tc.staged, completedWith(tc.completed...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Commit() error = %v, want it to contain %q", err, tc.want)
			}
			if written != "" {
				t.Fatalf("a refused commit wrote the transcript: %s", written)
			}
			if turn.manifest.Status != "streaming" {
				t.Fatalf("a refused commit moved the turn to %q", turn.manifest.Status)
			}
			if _, statErr := os.Stat(filepath.Join(turn.turnDir, finalFilename)); !os.IsNotExist(statErr) {
				t.Fatalf("a refused commit left a final record (%v)", statErr)
			}
		})
	}
}

func TestCommitWithoutOutputKeepsStagedItemsInIndexOrder(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-no-output")
	if err != nil {
		t.Fatal(err)
	}
	a, b := fcItem("fc_a", "call_a", "{}"), fcItem("fc_b", "call_b", "{}")
	for _, one := range []stagedItem{{4, b}, {2, a}} {
		if err := turn.StageOutputItem(outputItemEvent(one.index, one.item)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := turn.Commit([]byte(`{"type":"response.completed","response":{"id":"resp_n","status":"completed"}}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if first, second := strings.Index(string(data), `"fc_a"`), strings.Index(string(data), `"fc_b"`); first < 0 || second < first {
		t.Fatalf("items are not in index order: %s", data)
	}
}
