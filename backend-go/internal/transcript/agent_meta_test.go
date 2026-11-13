package transcript

import (
	"os"
	"strings"
	"testing"
)

func TestMetaValueReadsAnythingUnexpectedAsAbsent(t *testing.T) {
	base := `{"type":"message","role":"user","content":"x"`
	if value, ok := MetaValue([]byte(base+testMeta+`}`), MetaStoredAt, 0); !ok || string(value) != `"2026-11-13T07:01:02.345Z"` {
		t.Fatalf("well-formed = %s, %v", value, ok)
	}
	big := `{"` + MetaStoredAt + `":"` + strings.Repeat("x", 100) + `"}`
	for name, record := range map[string]string{
		"no meta":         base + `}`,
		"meta string":     base + `,"` + MetaField + `":"2026-11-13T07:01:02.345Z"}`,
		"meta array":      base + `,"` + MetaField + `":[{"` + MetaStoredAt + `":"x"}]}`,
		"meta null":       base + `,"` + MetaField + `":null}`,
		"unknown sub-key": base + `,"` + MetaField + `":{"other":1}}`,
		"over the cap":    base + `,"` + MetaField + `":` + big + `}`,
		"not an object":   `[1]`,
		"torn record":     base + testMeta,
		"empty":           ``,
	} {
		if value, ok := MetaValue([]byte(record), MetaStoredAt, 64); ok {
			t.Errorf("%s: read %s, want absent", name, value)
		}
	}
	// The cap is on the encoded meta value only: the same value under a larger cap is read.
	if _, ok := MetaValue([]byte(base+`,"`+MetaField+`":`+big+`}`), MetaStoredAt, len(big)); !ok {
		t.Fatal("a value at the cap was not read")
	}
	// A wrong type is returned as is; the caller checks it.
	if value, ok := MetaValue([]byte(base+`,"`+MetaField+`":{"`+MetaStoredAt+`":5}}`), MetaStoredAt, 0); !ok || string(value) != "5" {
		t.Fatalf("wrong type = %s, %v", value, ok)
	}
}

func TestEncodeMetaLeavesTheFieldOutOverTheCap(t *testing.T) {
	value := encodeMeta("2026-11-13T07:01:02.345Z", DefaultMetaMaxBytes)
	if string(value) != `{"stored_at":"2026-11-13T07:01:02.345Z"}` {
		t.Fatalf("encoded %s", value)
	}
	if got := encodeMeta("2026-11-13T07:01:02.345Z", len(value)-1); got != nil {
		t.Fatalf("over the cap encoded %s", got)
	}
}

func TestDropTrailingMetaRemovesOnlyALastMetaMember(t *testing.T) {
	plain := `{"type":"message","role":"user","content":"x","` + SessionInputSteerField + `":true}`
	for record, want := range map[string]string{
		plain[:len(plain)-1] + testMeta + `}`: plain,
		plain:                                 plain,
		`{"type":"x","` + MetaField + `":{"a":[1,"}"]} }`:                  `{"type":"x"}`,
		`{"` + MetaField + `":{}}`:                                         `{}`,
		`{"type":"response.completed","response":{"` + MetaField + `":1}}`: `{"type":"response.completed","response":{"` + MetaField + `":1}}`,
	} {
		got, err := DropTrailingMeta([]byte(record))
		if err != nil || string(got) != want {
			t.Errorf("DropTrailingMeta(%s) = %s, %v; want %s", record, got, err, want)
		}
	}
	for _, record := range []string{
		`{"` + MetaField + `":{},"type":"x"}`,
		`{"type":"x","` + MetaField + `":{},"` + SessionInputSteerField + `":true}`,
		`{"type":"x"`,
		`{"type":"x"} {}`,
		`not json`,
	} {
		if got, err := DropTrailingMeta([]byte(record)); err == nil {
			t.Errorf("DropTrailingMeta(%s) = %s, want an error", record, got)
		}
	}
}

// No caller may send a reservedFieldPrefix key, and no such top-level key reaches the request
// history, whatever its value: the batch fields, the steer marker, MetaField (also malformed)
// and a key no build knows yet. A nested key of that name is content and passes.
func TestReservedFieldsAreRefusedOnInputAndStrippedFromHistory(t *testing.T) {
	for _, key := range []string{MetaField, "__agent_x", SessionInputBatchIDField, SessionInputSteerField} {
		if err := ValidateInputItem(0, []byte(`{"type":"message","role":"user","content":"x","`+key+`":{}}`)); err == nil || !strings.Contains(err.Error(), "reserved __agent_ fields") {
			t.Errorf("%s: err = %v", key, err)
		}
	}
	nested := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"x","__agent_meta":{}}],"_agent_x":1}`)
	if err := ValidateInputItem(0, nested); err != nil {
		t.Fatalf("nested key refused: %v", err)
	}

	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":"plain"}`)); err != nil {
		t.Fatal(err)
	}
	clean, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(string(data), "}\n")
	for _, extra := range []string{
		testMeta,
		`,"` + MetaField + `":"malformed"`,
		`,"` + MetaField + `":{"` + MetaStoredAt + `":` + strings.Repeat("9", 70000) + `}`,
		`,"__agent_x":[1,2],"` + MetaField + `":{"unknown":true}`,
	} {
		if err := os.WriteFile(journal.path, []byte(line+extra+"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		items, err := store.ReadInputItems()
		if err != nil {
			t.Fatalf("%.40s: %v", extra, err)
		}
		if len(items) != 1 || string(items[0]) != string(clean[0]) {
			t.Fatalf("%.40s: history = %q, want %q", extra, items, clean)
		}
	}
}
