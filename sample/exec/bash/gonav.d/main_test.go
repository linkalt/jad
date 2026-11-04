package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture is a two-package tree with the shapes navigation must get right: a package-level func called
// from its own package and across an import, a method called on a value, a struct used as a type, an
// interface with one implementer, a local variable that shadows the symbol name, and tests.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"backend-go/a/a.go": `package a

import "fmt"

// Target is the symbol under test. It returns the width.
func Target(width int) int {
	return helper(width) + 1
}

func helper(width int) int {
	opts := Opts{Width: width}
	return opts.Width
}

// Opts holds one width.
type Opts struct {
	Width int
}

// Store is implemented by optsStore.
type Store interface {
	Get(key string) int
}

type optsStore struct {
	items map[string]int
}

func (s *optsStore) Get(key string) int {
	return s.items[key]
}

func withinA() int {
	return Target(2)
}

func describe(o Opts) string {
	return fmt.Sprint(o.Width)
}

func readOne(s *optsStore) int {
	return s.Get("k")
}
`,
		"backend-go/a/a_test.go": `package a

import "testing"

func TestTargetIsOneWider(t *testing.T) {
	if Target(1) != 2 {
		t.Fatal("wrong width")
	}
}

func TestSomethingElse(t *testing.T) {
	if helper(1) != 1 {
		t.Fatal("wrong width")
	}
}
`,
		"backend-go/b/b.go": `package b

import "jad/backend-go/a"

// Caller reaches Target through the import path.
func Caller() int {
	opts := a.Opts{Width: 3}
	return a.Target(opts.Width)
}

func local() int {
	return Caller()
}
`,
		"backend-go/b/b_extra.go": `package b

func second() int {
	return Caller()
}

func unrelated() int {
	target := 0
	return target
}
`,
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

// answer runs one command over root and returns its JSON answer as plain maps, so assertions read
// like the output an agent sees.
func answer(t *testing.T, root, cmd, symbol string, extra ...func(*config)) map[string]any {
	t.Helper()
	value := rawAnswer(t, root, cmd, symbol, extra...)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded
}

func rawAnswer(t *testing.T, root, cmd, symbol string, extra ...func(*config)) any {
	t.Helper()
	cfg := config{cmd: cmd, symbol: symbol, path: "backend-go", max: defaultMax, maxLines: defaultMaxLine, root: root}
	for _, apply := range extra {
		apply(&cfg)
	}
	value, err := execute(cfg, time.Now())
	if err == nil {
		return value
	}
	payload, ok := err.(*resultError)
	if !ok {
		t.Fatalf("%s %s: %v", cmd, symbol, err)
	}
	return payload.value
}

// run is answer over a fresh fixture.
func run(t *testing.T, cmd, symbol string, extra ...func(*config)) map[string]any {
	t.Helper()
	return answer(t, fixture(t), cmd, symbol, extra...)
}

func rows(value any) []map[string]any {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

func number(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return -1
}

func TestNavigateFindsExactDefinitionRange(t *testing.T) {
	got := run(t, "navigate", "Target")
	defs := rows(got["definitions"])
	if len(defs) != 1 {
		t.Fatalf("definitions = %d, want 1", len(defs))
	}
	if defs[0]["file"] != "backend-go/a/a.go" || number(defs[0]["start_line"]) != 6 || number(defs[0]["end_line"]) != 8 {
		t.Fatalf("definition = %v, want a/a.go 6-8", defs[0])
	}
	if defs[0]["signature"] != "func(width int) int" {
		t.Fatalf("signature = %v", defs[0]["signature"])
	}
	if defs[0]["doc"] != "Target is the symbol under test. It returns the width." {
		t.Fatalf("doc = %v", defs[0]["doc"])
	}
}

func TestNavigateResolvesCallersThroughImports(t *testing.T) {
	got := run(t, "navigate", "Target", func(cfg *config) { cfg.noTests = true })
	gotConfidence := map[string]string{}
	for _, caller := range rows(got["callers"]) {
		gotConfidence[caller["in"].(string)] = caller["confidence"].(string)
	}
	want := map[string]string{"withinA": "same_package", "Caller": "import"}
	if len(gotConfidence) != len(want) {
		t.Fatalf("callers = %v, want %v", gotConfidence, want)
	}
	for name, confidence := range want {
		if gotConfidence[name] != confidence {
			t.Fatalf("callers = %v, want %s resolved as %s", gotConfidence, name, confidence)
		}
	}
	for _, caller := range rows(got["callers"]) {
		if caller["in"] == "Caller" && (number(caller["call_line"]) != 8 || number(caller["start_line"]) != 6 || number(caller["end_line"]) != 9) {
			t.Fatalf("Caller site = %v, want line 8 inside the body 6-9", caller)
		}
	}
}

// A method call on a value cannot be typed without a type checker; it must say so instead of guessing.
func TestMethodCallOnAValueIsMarkedNameOnly(t *testing.T) {
	got := run(t, "navigate", "Get", func(cfg *config) { cfg.noTests = true })
	callers := rows(got["callers"])
	if len(callers) != 1 {
		t.Fatalf("callers = %v, want the call in readOne", callers)
	}
	if callers[0]["in"] != "readOne" || callers[0]["confidence"] != "name_only" {
		t.Fatalf("caller = %v, want readOne with name_only", callers[0])
	}
}

func TestNavigateIgnoresASameNamedLocal(t *testing.T) {
	got := run(t, "navigate", "Target", func(cfg *config) { cfg.noTests = true })
	for _, caller := range rows(got["callers"]) {
		if caller["in"] == "unrelated" {
			t.Fatalf("a local variable was reported as a caller: %v", caller)
		}
	}
}

func TestNavigateCountsTestFileCallersSeparately(t *testing.T) {
	got := run(t, "navigate", "Target")
	var sawCall, sawDecl bool
	for _, row := range rows(got["tests"]) {
		if row["kind"] == "call" && row["in"] == "TestTargetIsOneWider" {
			sawCall = true
		}
		if row["kind"] == "test_func" && row["in"] == "TestTargetIsOneWider" {
			sawDecl = true
		}
	}
	if !sawCall || !sawDecl {
		t.Fatalf("tests = %v, want the call and the test func that names Target", got["tests"])
	}
	counts := got["counts"].(map[string]any)
	if number(counts["callers"]) != 2 {
		t.Fatalf("non-test callers = %v, want withinA and Caller", counts["callers"])
	}
}

func TestNavigateResolvesCalleesInsideTheDefinition(t *testing.T) {
	got := run(t, "navigate", "Caller", func(cfg *config) { cfg.noTests = true })
	callees := rows(got["callees"])
	if len(callees) != 1 || callees[0]["name"] != "Target" || callees[0]["kind"] != "func" {
		t.Fatalf("callees = %v, want exactly the Target call", callees)
	}
	var related bool
	for _, row := range rows(got["types"]) {
		if row["name"] == "Opts" {
			related = true
		}
	}
	if !related {
		t.Fatalf("types = %v, want Opts from the composite literal", got["types"])
	}
}

func TestNavigateReportsInterfaceMethodsAndImplementers(t *testing.T) {
	got := run(t, "navigate", "Store", func(cfg *config) { cfg.noTests = true })
	defs := rows(got["definitions"])
	if len(defs) != 1 || defs[0]["kind"] != "interface" {
		t.Fatalf("definition = %v, want one interface", defs)
	}
	methods, _ := defs[0]["methods"].([]any)
	if len(methods) != 1 || methods[0] != "Get" {
		t.Fatalf("methods = %v, want [Get]", methods)
	}
	var implementer bool
	for _, row := range rows(got["types"]) {
		if row["name"] == "optsStore" {
			implementer = true
		}
	}
	if !implementer {
		t.Fatalf("types = %v, want optsStore as the implementer", got["types"])
	}
}

func TestNavigateOnATypeCollectsUsesNotJustCalls(t *testing.T) {
	got := run(t, "navigate", "Opts", func(cfg *config) { cfg.noTests = true })
	uses := rows(got["callers"])
	var inA, inB bool
	for _, row := range uses {
		switch row["file"] {
		case "backend-go/a/a.go":
			inA = true
		case "backend-go/b/b.go":
			inB = true
		}
		if row["kind"] != "ref" && row["kind"] != "selector_ref" {
			t.Fatalf("use = %v, want a type reference", row)
		}
	}
	if !inA || !inB {
		t.Fatalf("uses = %v, want the type named in both packages", uses)
	}
}

func TestSymbolIsAnExactLookupWithoutScan(t *testing.T) {
	got := run(t, "symbol", "helper")
	if _, ok := got["counts"]; ok {
		t.Fatalf("symbol must not run the reference scan, got counts: %v", got["counts"])
	}
	defs := rows(got["definitions"])
	if len(defs) != 1 || number(defs[0]["start_line"]) != 10 {
		t.Fatalf("definitions = %v, want helper at line 10", defs)
	}
}

func TestReceiverNarrowsSameNameMethods(t *testing.T) {
	got := run(t, "symbol", "Get", func(cfg *config) { cfg.receiver = "optsStore" })
	defs := rows(got["definitions"])
	if len(defs) != 1 || defs[0]["receiver"] != "*optsStore" {
		t.Fatalf("definitions = %v, want the optsStore method", defs)
	}
}

func TestMissingSymbolSuggestsAndFails(t *testing.T) {
	got := run(t, "navigate", "Targt")
	if got["ok"] != false {
		t.Fatalf("ok = %v, want false", got["ok"])
	}
	suggestions := got["suggestions"].([]any)
	if len(suggestions) == 0 || suggestions[0] != "Target" {
		t.Fatalf("suggestions = %v, want Target first for the typo Targt", suggestions)
	}
}

func TestTruncationIsReported(t *testing.T) {
	got := run(t, "navigate", "Caller", func(cfg *config) { cfg.noTests = true; cfg.max = 1 })
	truncated, ok := got["truncated"].(map[string]any)
	if !ok || truncated["callers"] != true {
		t.Fatalf("truncated = %v, want callers:true", got["truncated"])
	}
	if len(rows(got["callers"])) != 1 {
		t.Fatalf("callers = %v, want one bounded row", got["callers"])
	}
}

// TestRangesChainIntoSource is the chaining contract: the ranges of one answer are the input of the next.
func TestRangesChainIntoSource(t *testing.T) {
	root := fixture(t)
	nav := answer(t, root, "navigate", "Target")
	raw, err := json.Marshal(nav["ranges"])
	if err != nil {
		t.Fatalf("marshal ranges: %v", err)
	}
	var ranges []Range
	if err := json.Unmarshal(raw, &ranges); err != nil {
		t.Fatalf("unmarshal ranges: %v", err)
	}
	if len(ranges) < 2 {
		t.Fatalf("ranges = %v, want at least the definition and a caller", ranges)
	}
	value, err := sourceResult(config{cmd: "source", ranges: ranges, root: root, maxLines: defaultMaxLine}, time.Now())
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	text, _ := value["text"].(string)
	if !strings.Contains(text, "backend-go/a/a.go:6\tfunc Target(width int) int {") {
		t.Fatalf("source text missing the definition line:\n%s", text)
	}
	if !strings.Contains(text, "backend-go/b/b.go:8\t\treturn a.Target(opts.Width)") {
		t.Fatalf("source text missing the caller line:\n%s", text)
	}
}

func TestSourceBoundsOutput(t *testing.T) {
	value, err := sourceResult(config{
		cmd:      "source",
		ranges:   []Range{{File: "backend-go/b/b.go", Start: 1, End: 12}},
		root:     fixture(t),
		maxLines: 3,
	}, time.Now())
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if value["truncated"] != true || number(value["lines"]) != 3 {
		t.Fatalf("source = %v, want 3 lines and truncated", value)
	}
}

func TestSourceReportsAMissingFileWithoutLosingTheAnswer(t *testing.T) {
	value, err := sourceResult(config{
		cmd:      "source",
		ranges:   []Range{{File: "backend-go/nosuch.go", Start: 1, End: 2}},
		root:     fixture(t),
		maxLines: defaultMaxLine,
	}, time.Now())
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if text, _ := value["text"].(string); !strings.Contains(text, "no such file") {
		t.Fatalf("text = %q, want the read error inline", text)
	}
}

// TestDeterministicOutput guards the concurrent parse: the same tree must produce the same bytes.
func TestDeterministicOutput(t *testing.T) {
	root := fixture(t)
	call := func() string {
		value := rawAnswer(t, root, "navigate", "Target")
		value.(map[string]any)["scan"].(map[string]any)["ms"] = 0
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(encoded)
	}
	first := call()
	for i := 0; i < 5; i++ {
		if again := call(); again != first {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, again)
		}
	}
}

func TestParseArgs(t *testing.T) {
	cfg, err := parseArgs([]string{"navigate", "Target", "--max", "5", "--no-tests", "--path", "backend-go"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if cfg.cmd != "navigate" || cfg.symbol != "Target" || cfg.max != 5 || !cfg.noTests || cfg.path != "backend-go" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, err := parseArgs([]string{"navigate", "Target", "--max", "0"}); err == nil {
		t.Fatal("--max 0 must be refused")
	}
	if _, err := parseArgs([]string{"navigate", "Target", "--nope"}); err == nil {
		t.Fatal("an unknown flag must be refused")
	}
	if _, err := parseArgs([]string{"source", "a.go", "notanumber"}); err == nil {
		t.Fatal("a non-numeric START must be refused")
	}
}

func TestUnknownCommandAndMissingSymbolFail(t *testing.T) {
	if _, err := execute(config{cmd: "wat", root: fixture(t)}, time.Now()); err == nil {
		t.Fatal("unknown command must fail")
	}
	if _, err := execute(config{cmd: "navigate", root: fixture(t), path: "backend-go"}, time.Now()); err == nil {
		t.Fatal("navigate without a symbol must fail")
	}
	if _, err := execute(config{cmd: "source", root: fixture(t), maxLines: defaultMaxLine}, time.Now()); err == nil {
		t.Fatal("source without ranges must fail")
	}
}
