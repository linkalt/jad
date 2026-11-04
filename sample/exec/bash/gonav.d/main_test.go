package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// fixture2 covers struct fields, const/var uses, same-name symbols in two packages and test-only definitions.
func fixture2(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"backend-go/p/p.go": `package p

const Limit = 10

var counter int

type Config struct {
	maxLines int
	Name     string
}

func (c *Config) Lines() int {
	if c.maxLines > Limit {
		counter++
	}
	return c.maxLines
}

func New() Config {
	return Config{maxLines: Limit}
}

func wait() int { return Limit }

func useWait() int { return wait() }
`,
		"backend-go/p/p_test.go": `package p

import "testing"

type fixture struct{}

func (fixture) helperOnlyInTests() int { return Limit }

func wait2() int { return 0 }

func TestLimit(t *testing.T) {
	if New().maxLines != Limit {
		t.Fatal("x")
	}
}
`,
		"backend-go/q/q.go": `package q

import "jad/backend-go/p"

func wait() int { return p.Limit }

func useWait() int { return wait() }

func useCounter() int { return p.Limit + 1 }
`,
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func textOf(t *testing.T, root string, cfg config) string {
	t.Helper()
	cfg.root = root
	if cfg.path == "" {
		cfg.path = "backend-go"
	}
	if cfg.max == 0 {
		cfg.max = defaultMax
	}
	if cfg.maxLines == 0 {
		cfg.maxLines = defaultMaxLine
	}
	text, err := executeText(cfg, time.Now())
	if err != nil {
		if pe, ok := err.(*resultError); ok {
			return pe.value["text"].(string)
		}
		t.Fatalf("executeText: %v", err)
	}
	return text
}

func TestFieldIsIndexedAndItsUsesAreFound(t *testing.T) {
	root := fixture2(t)
	got := answer(t, root, "navigate", "Config.maxLines")
	defs := rows(got["definitions"])
	if len(defs) != 1 || defs[0]["kind"] != "field" || number(defs[0]["start_line"]) != 8 {
		t.Fatalf("definitions = %v, want the maxLines field at line 8", defs)
	}
	callers := rows(got["callers"])
	var in []string
	for _, c := range callers {
		in = append(in, c["in"].(string))
	}
	if strings.Join(in, ",") != "Config.Lines,Config.Lines,New" && strings.Join(in, ",") != "Config.Lines,New" {
		t.Fatalf("callers = %v, want Config.Lines and New", in)
	}
	if bare := answer(t, root, "symbol", "maxLines"); number(bare["count"]) != 1 {
		t.Fatalf("bare field name must resolve: %v", bare)
	}
}

func TestConstAndVarUsesAreCollected(t *testing.T) {
	root := fixture2(t)
	got := answer(t, root, "navigate", "p.Limit")
	files := map[string]bool{}
	for _, c := range rows(got["callers"]) {
		files[c["file"].(string)] = true
	}
	if !files["backend-go/p/p.go"] || !files["backend-go/q/q.go"] {
		t.Fatalf("callers = %v, want uses in p.go and (through the import) q.go", got["callers"])
	}
	if len(rows(got["tests"])) == 0 {
		t.Fatalf("tests = %v, want the use in p_test.go", got["tests"])
	}
	counter := answer(t, root, "navigate", "counter")
	if number(counter["counts"].(map[string]any)["callers"]) != 1 {
		t.Fatalf("var counter uses = %v, want 1", counter["counts"])
	}
}

func TestSameNameInDifferentPackagesIsNotMerged(t *testing.T) {
	root := fixture2(t)
	got := answer(t, root, "navigate", "wait")
	if got["ambiguous"] != true || len(rows(got["candidates"])) != 2 {
		t.Fatalf("want 2 candidates, got %v", got)
	}
	one := answer(t, root, "navigate", "q.wait")
	if one["ambiguous"] == true || len(rows(one["definitions"])) != 1 {
		t.Fatalf("q.wait must resolve to one definition: %v", one)
	}
	callers := rows(one["callers"])
	if len(callers) != 1 || callers[0]["file"] != "backend-go/q/q.go" {
		t.Fatalf("q.wait callers = %v, want only the q package use", callers)
	}
	m := answer(t, root, "symbol", "Config.Lines")
	if number(m["count"]) != 1 {
		t.Fatalf("Type.Method must resolve: %v", m)
	}
}

func TestNoTestsDropsDefinitionsInTestFiles(t *testing.T) {
	root := fixture2(t)
	with := answer(t, root, "symbol", "wait2")
	if number(with["count"]) != 1 {
		t.Fatalf("wait2 should be defined in the test file: %v", with)
	}
	without := answer(t, root, "symbol", "wait2", func(c *config) { c.noTests = true })
	if without["ok"] != false {
		t.Fatalf("--no-tests must drop test-file definitions: %v", without)
	}
	if text := textOf(t, root, config{cmd: "symbol", symbol: "helperOnlyInTests", noTests: true}); !strings.HasPrefix(text, "error:") {
		t.Fatalf("text = %q", text)
	}
}

func TestMultiSymbolInOneCall(t *testing.T) {
	root := fixture2(t)
	got := answer(t, root, "symbol", "", func(c *config) { c.symbols = splitSymbols("Limit|counter,Nope") })
	results := rows(got["results"])
	if len(results) != 3 || results[2]["ok"] != false || number(results[0]["count"]) != 1 {
		t.Fatalf("results = %v", got["results"])
	}
	cfg, err := parseArgs([]string{"symbol", "a|b", "c"})
	if err != nil || strings.Join(cfg.symbols, ",") != "a,b,c" {
		t.Fatalf("cfg = %+v err = %v", cfg, err)
	}
}

func TestShowPrintsTheBodyInOneCall(t *testing.T) {
	root := fixture2(t)
	got := textOf(t, root, config{cmd: "show", symbol: "p.New", symbols: []string{"p.New"}})
	want := "backend-go/p/p.go:19-21\n19\tfunc New() Config {\n20\t\treturn Config{maxLines: Limit}\n21\t}\n"
	if got != want {
		t.Fatalf("show =\n%q\nwant\n%q", got, want)
	}
	bounded := textOf(t, root, config{cmd: "show", symbols: []string{"Config.Lines"}, maxLines: 2})
	if !strings.Contains(bounded, "... truncated: ") || strings.Count(bounded, "\n") != 4 {
		t.Fatalf("show must honor max-lines: %q", bounded)
	}
	j := answer(t, root, "show", "p.New")
	if !strings.Contains(j["text"].(string), "backend-go/p/p.go:19\tfunc New() Config {") {
		t.Fatalf("json show = %v", j)
	}
}

func TestTextFormatGolden(t *testing.T) {
	root := fixture(t)
	got := textOf(t, root, config{cmd: "navigate", symbol: "Target", symbols: []string{"Target"}})
	want := "navigate Target: defs=1 callers=2 callees=1 types=0 tests=2 files=4 mentions=4\n" +
		"backend-go/a/a.go:6-8\tfunc\ta.Target\tfunc(width int) int\n" +
		"backend-go/a/a.go:33-35\tcaller\twithinA L34 same_package\n" +
		"backend-go/b/b.go:6-9\tcaller\tCaller L8 import\n" +
		"backend-go/a/a.go:10-13\tcallee:func\ta.helper\tfunc(width int) int\n" +
		"backend-go/a/a_test.go:5-9\ttest\tTestTargetIsOneWider\n" +
		"backend-go/a/a_test.go:5-9\ttest\tTestTargetIsOneWider L6 same_package\n"
	if got != want {
		t.Fatalf("navigate text =\n%s\nwant\n%s", got, want)
	}
	sym := textOf(t, root, config{cmd: "symbol", symbols: []string{"Opts"}})
	if sym != "symbol Opts: 1 definitions\nbackend-go/a/a.go:16-18\tstruct\ta.Opts\tfields=1\n" {
		t.Fatalf("symbol text = %q", sym)
	}
	src := textOf(t, root, config{cmd: "source", file: "backend-go/a/a.go", startLine: 6, endLine: 7})
	if src != "backend-go/a/a.go:6-7\n6\tfunc Target(width int) int {\n7\t\treturn helper(width) + 1\n" {
		t.Fatalf("source text = %q", src)
	}
	if strings.Contains(got, "ranges") {
		t.Fatal("text output must not carry a ranges section")
	}
}

func TestTextOutputIsCappedWithExactTotals(t *testing.T) {
	root := fixture(t)
	got := textOf(t, root, config{cmd: "navigate", symbols: []string{"Target"}, maxBytes: 200})
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "... truncated: showing ") || !strings.Contains(last, " of ") || !strings.Contains(last, "--path DIR") {
		t.Fatalf("last line = %q", last)
	}
	if len(got) > 200+len(last)+1 {
		t.Fatalf("output %d bytes exceeds the cap", len(got))
	}
}

func TestAmbiguousTextListsEachCandidateWithItsKey(t *testing.T) {
	got := textOf(t, fixture2(t), config{cmd: "symbol", symbols: []string{"wait"}})
	want := "symbol wait: ambiguous, 2 candidates; pass one key (pkg.Name or Type.Method)\n" +
		"backend-go/p/p.go:23-23\tfunc\tp.wait\tfunc() int\n" +
		"backend-go/q/q.go:5-5\tfunc\tq.wait\tfunc() int\n"
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

// fixtureModule is a self-contained module with every shape engine B must get right: a method whose
// name is also a package-level function, a field, a const, an interface implemented by a value and by
// a pointer, internal and external (x_test) test packages, a build-tagged file and a package that does
// not compile.
func fixtureModule(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module fixture\n\ngo 1.21\n",
		"pkg/pool/pool.go": `package pool

const Limit = 2

type Pool struct {
	open int
}

func (p *Pool) Close() error {
	p.open = 0
	return nil
}

func Close(p *Pool) error {
	return nil
}

func (p *Pool) Open() int {
	return p.open
}

func (p *Pool) Uses() int {
	return Limit
}

func (p *Pool) Name() string {
	return "pool"
}

type Closer interface {
	Close() error
}

type Namer interface {
	Name() string
}

type Conn struct{}

func (c Conn) Close() error {
	return nil
}

type bucket struct {
	open int
}

func (b *bucket) Reset() int {
	b.open = 0
	return b.open
}

type hidden struct{}

func (h *hidden) Close() error {
	return nil
}

type opener interface {
	Open() int
}

type Handler func(int) error

type Emitter interface {
	Emit(Handler) error
}
`,
		"pkg/pool/pool_test.go": `package pool

import "testing"

func TestPoolClose(t *testing.T) {
	p := &Pool{}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

type onlyTest struct{ n int }

func (o *onlyTest) Use() int {
	return o.n
}
`,
		"pkg/user/user.go": `package user

import "fixture/pkg/pool"

func Close(p *pool.Pool) error {
	return p.Close()
}

func UseLimit() int {
	return pool.Limit
}

func UseField(p *pool.Pool) int {
	return p.Open()
}

type Sender struct{}

func (s *Sender) Emit(h pool.Handler) error {
	return h(1)
}
`,
		"pkg/user/user_test.go": `package user

import (
	"testing"

	"fixture/pkg/pool"
)

func TestClose(t *testing.T) {
	if err := pool.Close(nil); err != nil {
		t.Fatal(err)
	}
}
`,
		"pkg/user/x_test.go": `package user_test

import (
	"testing"

	"fixture/pkg/pool"
	"fixture/pkg/user"
)

func TestExternal(t *testing.T) {
	p := &pool.Pool{}
	_ = p.Close()
	_ = user.UseLimit()
	_ = p.Open()
}
`,
		"pkg/tagged/tagged.go": `package tagged

func Live() int {
	return 1
}
`,
		"pkg/tagged/tagged_windows.go": `//go:build windows

package tagged

func OnlyWindows() int {
	return Live() + 1
}
`,
		"pkg/broken/broken.go": `package broken

func Broken() int {
	return undefinedSymbol
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

// typedText runs one engine B command over a fixture module and returns its text answer.
func refsText(t *testing.T, root, cmd string, symbols ...string) string {
	t.Helper()
	return refsTextOpts(t, root, config{cmd: cmd, symbols: symbols}, nil)
}

func refsTextOpts(t *testing.T, root string, cfg config, extra func(*config)) string {
	t.Helper()
	cfg.root = root
	cfg.path = "."
	if cfg.max == 0 {
		cfg.max = defaultMax
	}
	cfg.maxLines = defaultMaxLine
	if extra != nil {
		extra(&cfg)
	}
	text, err := executeText(cfg, time.Now())
	if err == nil {
		return steady(text)
	}
	if pe, ok := err.(*resultError); ok {
		if s, ok := pe.value["text"].(string); ok {
			return steady(s)
		}
	}
	t.Fatalf("%s %v: %v", cfg.cmd, cfg.symbols, err)
	return ""
}

// steady removes the one number a golden cannot pin: the elapsed milliseconds.
func steady(text string) string {
	re := regexp.MustCompile(`\d+ms`)
	return re.ReplaceAllString(text, "<ms>")
}

// typedAnswer runs one engine B command and returns its JSON answer as plain maps.
func refsJSON(t *testing.T, root, cmd string, symbols ...string) map[string]any {
	t.Helper()
	return refsJSONOpts(t, root, config{cmd: cmd, symbols: symbols}, nil)
}

func refsJSONOpts(t *testing.T, root string, cfg config, extra func(*config)) map[string]any {
	t.Helper()
	cfg.root = root
	cfg.path = "."
	cfg.max = defaultMax
	cfg.maxLines = defaultMaxLine
	if extra != nil {
		extra(&cfg)
	}
	value, err := execute(cfg, time.Now())
	if err != nil {
		payload, ok := err.(*resultError)
		if !ok {
			t.Fatalf("%s %v: %v", cfg.cmd, cfg.symbols, err)
		}
		value = payload.value
	}
	encoded, mErr := json.Marshal(value)
	if mErr != nil {
		t.Fatalf("marshal: %v", mErr)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded
}

func at(row map[string]any) string {
	return fmt.Sprintf("%s:%d:%d", row["file"], number(row["call_line"]), number(row["col"]))
}

// TestRefsResolveMethodsByReceiverNotName is the core of engine B: Pool.Close, Conn.Close, the
// package-level Close in pool and in user share one name and must never be mixed.
func TestRefsResolveMethodsByReceiverNotName(t *testing.T) {
	root := fixtureModule(t)
	got := refsJSON(t, root, "refs", "pool.Pool.Close")
	if got["types"] != "ok" {
		t.Fatalf("types = %v, want ok", got["types"])
	}
	refs := rows(got["refs"])
	if len(refs) != 1 || at(refs[0]) != "pkg/user/user.go:6:11" || refs[0]["in"] != "Close" {
		t.Fatalf("refs = %v, want only the Pool.Close call in user.Close", refs)
	}
	method := rows(got["definitions"])
	if len(method) != 1 || method[0]["kind"] != "method" {
		t.Fatalf("definitions = %v", method)
	}
	fn := refsJSON(t, root, "refs", "pool.Close")
	fnRefs := rows(fn["refs"])
	if len(fnRefs) != 0 {
		t.Fatalf("the package-level pool.Close has no prod callers, got %v", fnRefs)
	}
	if tests := rows(fn["tests"]); len(tests) != 1 || at(tests[0]) != "pkg/user/user_test.go:10:17" {
		t.Fatalf("tests = %v, want the user_test.go call of the package-level Close", tests)
	}
	ambiguous := refsJSON(t, root, "refs", "Close")
	if ambiguous["ambiguous"] != true || len(rows(ambiguous["candidates"])) != 5 {
		t.Fatalf("Close must be ambiguous across the three methods and the two functions: %v", ambiguous["candidates"])
	}
}

func TestRefsResolveFieldsAndConsts(t *testing.T) {
	root := fixtureModule(t)
	field := refsJSON(t, root, "refs", "pool.Pool.open")
	refs := rows(field["refs"])
	if len(refs) != 2 || refs[0]["kind"] != "field" {
		t.Fatalf("field refs = %v, want the two uses in Pool.Close and Pool.Open", refs)
	}
	if in := refs[0]["in"]; in != "Pool.Close" {
		t.Fatalf("in = %v, want Pool.Close", in)
	}
	konst := refsJSON(t, root, "refs", "pool.Limit")
	var ins []string
	for _, r := range rows(konst["refs"]) {
		if r["kind"] != "const" {
			t.Fatalf("kind = %v, want const (no call site here)", r["kind"])
		}
		ins = append(ins, r["in"].(string))
	}
	if strings.Join(ins, ",") != "Pool.Uses,UseLimit" {
		t.Fatalf("const uses = %v, want Pool.Uses and UseLimit", ins)
	}
}

func TestRefsGroupTestsAndIncludeXTestPackage(t *testing.T) {
	root := fixtureModule(t)
	got := refsJSON(t, root, "refs", "pool.Pool.Close")
	tests := rows(got["tests"])
	var files []string
	for _, s := range tests {
		files = append(files, s["file"].(string))
	}
	if strings.Join(files, ",") != "pkg/pool/pool_test.go,pkg/user/x_test.go" {
		t.Fatalf("test refs = %v, want the internal and the external test package", files)
	}
	noTests := refsJSONOpts(t, root, config{cmd: "refs", symbols: []string{"pool.Pool.Close"}}, func(c *config) { c.noTests = true })
	if len(rows(noTests["tests"])) != 0 || number(noTests["counts"].(map[string]any)["callers"]) != 1 {
		t.Fatalf("--no-tests must drop test refs and x_test packages: %v", noTests["counts"])
	}
}

func TestImplAnswersBothDirectionsWithPointerAndValue(t *testing.T) {
	root := fixtureModule(t)
	iface := refsJSON(t, root, "impl", "pool.Closer")
	got := map[string]string{}
	for _, h := range rows(iface["impls"]) {
		got[h["name"].(string)] = h["satisfies"].(string)
	}
	if got["Pool"] != "pointer" || got["Conn"] != "both" || got["hidden"] != "pointer" || len(got) != 3 {
		t.Fatalf("implementers of Closer = %v, want Pool, Conn and the unexported hidden", got)
	}
	reverse := refsJSON(t, root, "impl", "pool.Pool")
	satisfied := map[string]string{}
	for _, h := range rows(reverse["impls"]) {
		satisfied[h["name"].(string)] = h["satisfies"].(string)
	}
	if satisfied["Closer"] != "pointer" || satisfied["Namer"] != "pointer" || satisfied["opener"] != "pointer" || len(satisfied) != 3 {
		t.Fatalf("interfaces satisfied by Pool = %v, want Closer, Namer and opener through *Pool", satisfied)
	}
	unexported := refsText(t, root, "impl", "pool.opener")
	if !strings.Contains(unexported, "pool.Pool") || !strings.Contains(unexported, "not exported") {
		t.Fatalf("an unexported interface must resolve from source and say so:\n%s", unexported)
	}
	notAType := refsJSON(t, root, "impl", "pool.Limit")
	if notAType["ok"] != false {
		t.Fatalf("impl on a const must fail: %v", notAType)
	}
}

func TestBuildTaggedFilesAreNameOnly(t *testing.T) {
	root := fixtureModule(t)
	got := refsJSON(t, root, "refs", "tagged.Live")
	if len(rows(got["refs"])) != 0 {
		t.Fatalf("refs = %v, want none in compiled files", got["refs"])
	}
	name := rows(got["name_only"])
	if len(name) != 1 || name[0]["file"] != "pkg/tagged/tagged_windows.go" || name[0]["confidence"] != "name_only" {
		t.Fatalf("name_only = %v, want the build-tagged call", name)
	}
}

func TestTypeErrorsReportPartialNotSilence(t *testing.T) {
	root := fixtureModule(t)
	got := refsJSON(t, root, "refs", "broken.Broken")
	if got["types"] != "partial" {
		t.Fatalf("types = %v, want partial", got["types"])
	}
	if problems, _ := got["problems"].([]any); len(problems) == 0 {
		t.Fatalf("a partial answer must name its problems: %v", got)
	}
	text := refsText(t, root, "refs", "broken.Broken")
	if !strings.Contains(text, "types=partial") {
		t.Fatalf("text must say types=partial:\n%s", text)
	}
}

func TestRefsGoldenText(t *testing.T) {
	root := fixtureModule(t)
	got := refsText(t, root, "refs", "pool.Pool.Close")
	want := `# refs Close (*Pool): 3 refs, shown 3, tests=2, types=ok, <ms>
pkg/user/user.go:6:11	call	in=Close
# tests: 2 (grouped; --no-tests skips them)
pkg/pool/pool_test.go:7:14	call	in=TestPoolClose
pkg/user/x_test.go:12:8	call	in=TestExternal
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	impl := refsText(t, root, "impl", "pool.Closer")
	wantImpl := `# impl Closer (pool): 3 implementers, shown 3, types=ok, <ms>
pkg/pool/pool.go:5-7	struct	pool.Pool	satisfies=pointer
pkg/pool/pool.go:38-38	struct	pool.Conn	satisfies=both
pkg/pool/pool.go:53-53	struct	pool.hidden	satisfies=pointer
`
	if impl != wantImpl {
		t.Fatalf("impl text\n%s\nwant\n%s", impl, wantImpl)
	}
}

func TestCallersAliasKeepsOnlyCalls(t *testing.T) {
	root := fixtureModule(t)
	// Pool.Name is only ever used as a method value here, never called, so --calls must drop it.
	if err := os.WriteFile(filepath.Join(root, "pkg/user/named.go"), []byte(`package user

import "fixture/pkg/pool"

func NameOf(p *pool.Pool) string {
	return p.Name()
}

func NamerValue(p *pool.Pool) func() string {
	return p.Name
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	all := refsJSON(t, root, "refs", "pool.Pool.Name")
	if n := number(all["counts"].(map[string]any)["refs"]); n != 2 {
		t.Fatalf("refs = %v, want the call and the method value", all["counts"])
	}
	calls := refsJSON(t, root, "callers", "pool.Pool.Name")
	refs := rows(calls["refs"])
	if len(refs) != 1 || refs[0]["in"] != "NameOf" {
		t.Fatalf("callers = %v, want only NameOf", refs)
	}
	flag := refsJSON(t, root, "refs", "pool.Pool.Name")
	_ = flag
	viaFlag := refsJSONOpts(t, root, config{cmd: "refs", symbols: []string{"pool.Pool.Name"}, calls: true}, nil)
	if len(rows(viaFlag["refs"])) != 1 || viaFlag["cmd"] != "refs" {
		t.Fatalf("--calls must behave like the callers alias: %v", viaFlag["refs"])
	}
}

func TestNoTestsDropsTestFileDefinitions(t *testing.T) {
	root := fixtureModule(t)
	with := refsJSON(t, root, "refs", "pool.onlyTest.Use")
	if with["ok"] != true {
		t.Fatalf("onlyTest.Use is defined in a test file: %v", with)
	}
	without := refsJSONOpts(t, root, config{cmd: "refs", symbols: []string{"pool.onlyTest.Use"}}, func(c *config) { c.noTests = true })
	if without["ok"] != false {
		t.Fatalf("--no-tests must drop test-file definitions: %v", without)
	}
}

func TestEngineBFallsBackLoudlyWithNoModule(t *testing.T) {
	// fixture(t) is a backend-go style tree without a go.mod, so engine B cannot run there.
	got := answer(t, fixture(t), "refs", "Target")
	if got["types"] != "unavailable" {
		t.Fatalf("types = %v, want unavailable", got["types"])
	}
	note, _ := got["note"].(string)
	if !strings.Contains(note, "engine B unavailable") {
		t.Fatalf("note = %q, must say why engine B did not run", note)
	}
	refs := rows(got["refs"])
	if len(refs) == 0 {
		t.Fatalf("the fallback must still answer: %v", got)
	}
	for _, r := range refs {
		if r["confidence"] != "name_only" {
			t.Fatalf("fallback hits must be name_only: %v", r)
		}
	}
}

func TestUsageErrorsExitWithCodeTwo(t *testing.T) {
	root := fixtureModule(t)
	_, err := executeText(config{cmd: "refs", root: root, path: ".", max: defaultMax, maxLines: defaultMaxLine}, time.Now())
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Fatalf("refs without a symbol must be a usage error, got %v", err)
	}
	if _, err := executeText(config{cmd: "wat", symbol: "x", root: root, path: ".", max: defaultMax}, time.Now()); !errors.As(err, &usage) {
		t.Fatalf("an unknown command must be a usage error, got %v", err)
	}
	if _, err := parseArgs([]string{"refs", "x", "--nope"}); err == nil {
		t.Fatal("an unknown flag must fail")
	}
	// An unknown symbol is an answer, not a usage error: exit code 1.
	sym := refsJSON(t, root, "refs", "NoSuchSymbol")
	if sym["ok"] != false {
		t.Fatalf("want a not-found answer, got %v", sym)
	}
}

// fixtureChecker loads the fixture module the way engine B does, so a test can hold exported objects.
func fixtureChecker(t *testing.T, root string) (*checker, *loader) {
	t.Helper()
	ld, err := loadModule(root, ".", true)
	if err != nil {
		t.Fatalf("loadModule: %v", err)
	}
	return newChecker(ld), ld
}

func methodOf(t *testing.T, pkg *types.Package, typeName, method string) types.Object {
	t.Helper()
	named, ok := pkg.Scope().Lookup(typeName).Type().(*types.Named)
	if !ok {
		t.Fatalf("%s is not a named type", typeName)
	}
	for i := 0; i < named.NumMethods(); i++ {
		if named.Method(i).Name() == method {
			return named.Method(i)
		}
	}
	t.Fatalf("%s has no method %s", typeName, method)
	return nil
}

// TestTargetSpecMatchesStructurally pins the rules that decide a hit when object identity cannot: the
// same name in the same package is not enough, the receiver and the kind are part of the symbol.
func TestTargetSpecMatchesStructurally(t *testing.T) {
	root := fixtureModule(t)
	ck, _ := fixtureChecker(t, root)
	poolPkg, err := ck.gc.Import("fixture/pkg/pool")
	if err != nil {
		t.Fatalf("import pool: %v", err)
	}
	userPkg, err := ck.gc.Import("fixture/pkg/user")
	if err != nil {
		t.Fatalf("import user: %v", err)
	}
	poolClose := methodOf(t, poolPkg, "Pool", "Close")
	connClose := methodOf(t, poolPkg, "Conn", "Close")
	funcClose := poolPkg.Scope().Lookup("Close")
	userClose := userPkg.Scope().Lookup("Close")
	method := newTargetSpec("fixture/pkg/pool", []Decl{{Name: "Close", Kind: "method", Receiver: "*Pool"}})
	if !method.match(ck, poolClose) {
		t.Fatal("Pool.Close must match its own method")
	}
	if method.match(ck, connClose) {
		t.Fatal("Pool.Close must not match Conn.Close: methods resolve by receiver, not by name")
	}
	if method.match(ck, funcClose) {
		t.Fatal("a method spec must not match the package-level function of the same name")
	}
	if method.match(ck, userClose) {
		t.Fatal("Pool.Close must not match user.Close: different package")
	}
	fn := newTargetSpec("fixture/pkg/pool", []Decl{{Name: "Close", Kind: "func"}})
	if !fn.match(ck, funcClose) || fn.match(ck, poolClose) || fn.match(ck, userClose) {
		t.Fatal("the package-level pool.Close must match only itself")
	}
	field := newTargetSpec("fixture/pkg/pool", []Decl{{Name: "open", Kind: "field", Receiver: "Pool"}})
	if field.match(ck, funcClose) || field.match(ck, poolClose) {
		t.Fatal("a field spec must not match functions")
	}
	konst := newTargetSpec("fixture/pkg/pool", []Decl{{Name: "Limit", Kind: "const"}})
	if !konst.match(ck, poolPkg.Scope().Lookup("Limit")) {
		t.Fatal("Limit must match its own const")
	}
	if konst.match(ck, userPkg.Scope().Lookup("UseLimit")) {
		t.Fatal("Limit must not match a same-named symbol of another package")
	}
}

// TestImplCrossesPackagesWithForeignTypes is the identity trap: an implementer in another package
// mentions a type of the interface's package, so both sides must be compared in one identity.
func TestImplCrossesPackagesWithForeignTypes(t *testing.T) {
	root := fixtureModule(t)
	forward := refsJSON(t, root, "impl", "pool.Emitter")
	impls := rows(forward["impls"])
	if len(impls) != 1 || impls[0]["name"] != "Sender" || impls[0]["satisfies"] != "pointer" {
		t.Fatalf("implementers of Emitter = %v, want user.Sender through *Sender", impls)
	}
	reverse := refsJSON(t, root, "impl", "user.Sender")
	satisfied := rows(reverse["impls"])
	if len(satisfied) != 1 || satisfied[0]["name"] != "Emitter" || satisfied[0]["satisfies"] != "pointer" {
		t.Fatalf("interfaces satisfied by Sender = %v, want pool.Emitter", satisfied)
	}
	if forward["types"] != "ok" || reverse["types"] != "ok" {
		t.Fatalf("both directions must be exact, got %v / %v", forward["types"], reverse["types"])
	}
}

// writeModule writes a one-package module, for shapes the shared fixtures do not cover.
func writeModule(t *testing.T, pkgSource string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{"go.mod": "module fixture\n\ngo 1.21\n", "a/a.go": pkgSource}
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

// A package-level var or const must not match a same-named local: refs lists only the package-level uses.
func TestRefsVarIgnoresShadowingLocal(t *testing.T) {
	root := writeModule(t, `package a

var Count = 1

func F() int {
	Count := 2
	return Count
}

func G() int {
	return Count
}

const Limit = 1

func H() int {
	const Limit = 2
	return Limit
}

func K() int {
	return Limit
}
`)
	got := refsJSON(t, root, "refs", "a.Count")
	refs := rows(got["refs"])
	if len(refs) != 1 || refs[0]["in"] != "G" {
		t.Fatalf("refs = %v, want only the package-level use in G (not the local in F)", refs)
	}
	konst := refsJSON(t, root, "refs", "a.Limit")
	if refs := rows(konst["refs"]); len(refs) != 1 || refs[0]["in"] != "K" {
		t.Fatalf("refs = %v, want only the package-level const use in K (not the local in H)", refs)
	}
}

// A qualified Type.field must not match a field of a function-local struct that happens to share the name.
func TestRefsFieldIgnoresLocalStructWithSameField(t *testing.T) {
	root := writeModule(t, `package a

type T struct{ Name string }

func Use(t T) string { return t.Name }

func G() string {
	type local struct{ Name string }
	var l local
	return l.Name
}
`)
	got := refsJSON(t, root, "refs", "a.T.Name")
	for _, ref := range rows(got["refs"]) {
		if ref["in"] == "G" {
			t.Fatalf("refs lists G's local.Name for a.T.Name: %v", ref)
		}
	}
	if refs := rows(got["refs"]); len(refs) != 1 || refs[0]["in"] != "Use" {
		t.Fatalf("refs = %v, want only the T.Name use in Use", refs)
	}
}
