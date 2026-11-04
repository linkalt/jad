// gonav: deterministic Go navigation from the compiler's own parser (go/ast). Stdlib only.
// Prints exactly one JSON object; every range is {"file","start_line","end_line"} relative to the
// workspace root, so any answer chains into "gonav source" (or the bash helper read.sh).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- output contract ----------

// Range is the one shape every section shares. It is what makes answers chainable.
type Range struct {
	File  string `json:"file"`
	Start int    `json:"start_line"`
	End   int    `json:"end_line"`
}

// Decl is a definition: the queried symbol, a callee or a related type.
type Decl struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Package   string   `json:"package,omitempty"`
	Receiver  string   `json:"receiver,omitempty"`
	Signature string   `json:"signature,omitempty"`
	Doc       string   `json:"doc,omitempty"`
	Fields    int      `json:"fields,omitempty"`
	Methods   []string `json:"methods,omitempty"`
	Range
}

// Site is a reference: a call site, a package-qualified call, a method call or a file with counts.
type Site struct {
	In         string `json:"in,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Qualifier  string `json:"qualifier,omitempty"`
	CallLine   int    `json:"call_line,omitempty"`
	Col        int    `json:"col,omitempty"`
	Mentions   int    `json:"mentions,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Range
}

// ---------- configuration ----------

const (
	defaultMax     = 40
	maxCap         = 200
	defaultMaxLine = 400
	textCapBytes   = 4096
	maxSuggestions = 8
	maxCallersPer  = 40
)

type config struct {
	cmd       string
	symbol    string
	symbols   []string
	json      bool
	maxBytes  int
	path      string
	receiver  string
	max       int
	maxLines  int
	noTests   bool
	calls     bool
	file      string
	startLine int
	endLine   int
	ranges    []Range
	root      string
}

func main() {
	started := time.Now()
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		// Every parse failure is a usage error, which exits 2.
		exitFor(cfg, &usageError{err: err})
	}
	if !cfg.json {
		text, err := executeText(cfg, started)
		if err != nil {
			var payload *resultError
			if errors.As(err, &payload) {
				fmt.Print(capText(renderError(payload.value), cfg, ""))
				os.Exit(1)
			}
			exitFor(cfg, err)
		}
		fmt.Print(text)
		return
	}
	value, err := execute(cfg, started)
	if err != nil {
		var payload *resultError
		if errors.As(err, &payload) {
			emit(payload.value)
			os.Exit(1)
		}
		exitFor(cfg, err)
	}
	emit(value)
}

// execute builds the one JSON answer of a call. Printing and exiting happen only in main, which is
// what lets the commands be tested directly.
func execute(cfg config, started time.Time) (any, error) {
	switch cfg.cmd {
	case "navigate", "symbol", "impact":
		return symbolResult(cfg, started)
	case "show":
		return showResult(cfg, started)
	case "refs", "callers", "impl":
		return typedResult(cfg, started)
	case "source":
		return sourceResult(cfg, started)
	}
	return nil, usagef("unknown command %q; use navigate, symbol, impact, show, refs, callers, impl or source", cfg.cmd)
}

// usageError is a bad invocation, which exits 2 and never becomes an answer.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }

func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

// resultError is a complete answer that reports its own failure, such as a symbol that does not exist.
type resultError struct{ value map[string]any }

func (e *resultError) Error() string {
	text, _ := e.value["error"].(string)
	return text
}

func emit(value any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

// exitFor maps an error to the documented exit codes: 2 for a bad invocation, 1 otherwise.
func exitFor(cfg config, err error) {
	code := 1
	var usage *usageError
	if errors.As(err, &usage) {
		code = 2
	}
	if !cfg.json {
		fmt.Printf("error: %v\n", err)
		os.Exit(code)
	}
	emit(map[string]any{"ok": false, "cmd": cfg.cmd, "error": err.Error()})
	os.Exit(code)
}

func fail(cmd string, err error) {
	emit(map[string]any{"ok": false, "cmd": cmd, "error": err.Error()})
	os.Exit(1)
}

// ---------- arguments and stdin ----------

func parseArgs(args []string) (config, error) {
	cfg := config{max: defaultMax, maxLines: defaultMaxLine}
	cfg.root = os.Getenv("GONAV_ROOT")
	if cfg.root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return cfg, err
		}
		cfg.root = wd
	}
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch a {
		case "--path", "-p":
			v, err := value()
			if err != nil {
				return cfg, err
			}
			cfg.path = v
		case "--receiver", "-r":
			v, err := value()
			if err != nil {
				return cfg, err
			}
			cfg.receiver = strings.TrimPrefix(v, "*")
		case "--root":
			v, err := value()
			if err != nil {
				return cfg, err
			}
			cfg.root = v
		case "--max", "-m":
			v, err := value()
			if err != nil {
				return cfg, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return cfg, fmt.Errorf("--max needs a positive integer")
			}
			cfg.max = n
		case "--max-lines":
			v, err := value()
			if err != nil {
				return cfg, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return cfg, fmt.Errorf("--max-lines needs a positive integer")
			}
			cfg.maxLines = n
		case "--max-bytes":
			v, err := value()
			if err != nil {
				return cfg, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return cfg, fmt.Errorf("--max-bytes needs a positive integer")
			}
			cfg.maxBytes = n
		case "--calls":
			cfg.calls = true
		case "--no-tests":
			cfg.noTests = true
		case "--json":
			cfg.json = true
		case "--help", "-h":
			return cfg, fmt.Errorf("usage: gonav navigate|symbol|impact|show|refs|callers|impl SYM [SYM...] (SYM: Name, Type.Field, Type.Method, pkg.Name; a|b|c ok) [--path DIR] [--receiver R] [--max N] [--max-lines N] [--max-bytes N] [--no-tests] [--calls] [--json] | gonav source [FILE START END] (JSON ranges on stdin)")
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				return cfg, fmt.Errorf("unknown flag %q", a)
			}
			positional = append(positional, a)
		}
	}
	if err := applyPositional(&cfg, positional); err != nil {
		return cfg, err
	}
	if err := applyStdin(&cfg); err != nil {
		return cfg, err
	}
	if abs, err := filepath.Abs(cfg.root); err == nil {
		cfg.root = abs
	}
	if cfg.max > maxCap {
		cfg.max = maxCap
	}
	return cfg, nil
}

func applyPositional(cfg *config, positional []string) error {
	if len(positional) == 0 {
		return nil
	}
	cfg.cmd = positional[0]
	switch cfg.cmd {
	case "source":
		if len(positional) > 1 {
			cfg.file = positional[1]
		}
		if len(positional) > 2 {
			n, err := strconv.Atoi(positional[2])
			if err != nil {
				return fmt.Errorf("START must be a line number, got %q", positional[2])
			}
			cfg.startLine = n
		}
		if len(positional) > 3 {
			n, err := strconv.Atoi(positional[3])
			if err != nil {
				return fmt.Errorf("END must be a line number, got %q", positional[3])
			}
			cfg.endLine = n
		}
	default:
		for _, arg := range positional[1:] {
			cfg.symbols = append(cfg.symbols, splitSymbols(arg)...)
		}
		if len(cfg.symbols) > 0 {
			cfg.symbol = cfg.symbols[0]
		}
	}
	return nil
}

// applyStdin merges the JSON input object; explicit argv wins and stdin only fills what argv left empty.
func applyStdin(cfg *config) error {
	stat, err := os.Stdin.Stat()
	if err != nil || stat.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var in struct {
		Cmd       string   `json:"cmd"`
		Action    string   `json:"action"`
		Symbol    string   `json:"symbol"`
		Symbols   []string `json:"symbols"`
		JSON      bool     `json:"json"`
		MaxBytes  int      `json:"max_bytes"`
		Name      string   `json:"name"`
		Path      string   `json:"path"`
		Receiver  string   `json:"receiver"`
		Max       int      `json:"max"`
		MaxLines  int      `json:"max_lines"`
		NoTests   bool     `json:"no_tests"`
		File      string   `json:"file"`
		StartLine int      `json:"start_line"`
		EndLine   int      `json:"end_line"`
		Range     *Range   `json:"range"`
		Ranges    []Range  `json:"ranges"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("stdin is not a JSON object: %v", err)
	}
	if cfg.cmd == "" {
		cfg.cmd = firstNonEmpty(in.Cmd, in.Action)
	}
	if cfg.symbol == "" && len(cfg.symbols) == 0 {
		cfg.symbol = firstNonEmpty(in.Symbol, in.Name)
		for _, s := range append([]string{cfg.symbol}, in.Symbols...) {
			cfg.symbols = append(cfg.symbols, splitSymbols(s)...)
		}
		if len(cfg.symbols) > 0 {
			cfg.symbol = cfg.symbols[0]
		}
	}
	if in.JSON {
		cfg.json = true
	}
	if cfg.maxBytes == 0 && in.MaxBytes > 0 {
		cfg.maxBytes = in.MaxBytes
	}
	if cfg.path == "" {
		cfg.path = in.Path
	}
	if cfg.receiver == "" {
		cfg.receiver = strings.TrimPrefix(in.Receiver, "*")
	}
	if cfg.max == defaultMax && in.Max > 0 {
		cfg.max = in.Max
	}
	if cfg.maxLines == defaultMaxLine && in.MaxLines > 0 {
		cfg.maxLines = in.MaxLines
	}
	if in.NoTests {
		cfg.noTests = true
	}
	if cfg.file == "" {
		cfg.file = in.File
	}
	if cfg.startLine == 0 {
		cfg.startLine = in.StartLine
	}
	if cfg.endLine == 0 {
		cfg.endLine = in.EndLine
	}
	if len(cfg.ranges) == 0 {
		cfg.ranges = in.Ranges
		if in.Range != nil {
			cfg.ranges = append(cfg.ranges, *in.Range)
		}
	}
	if cfg.cmd == "" {
		cfg.cmd = "navigate"
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------- index (pass 1: declarations) ----------

type fileFact struct {
	rel string
	pkg string
}

type index struct {
	fset    *token.FileSet
	root    string
	scanDir string
	files   []fileFact
	decls   []Decl
	byName  map[string][]int // declared name -> decl indices
	typeIdx map[string][]int // type name -> decl indices
	dirs    map[string]bool  // repo-relative package directories seen
}

func newIndex(root, scanDir string) (*index, error) {
	abs := scanDir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, scanDir)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("path %s: %v", scanDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path %s is not a directory", scanDir)
	}
	files, err := collectGoFiles(abs)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .go files under %s", scanDir)
	}
	ix := &index{
		fset:    token.NewFileSet(),
		root:    root,
		scanDir: scanDir,
		files:   make([]fileFact, len(files)),
		byName:  map[string][]int{},
		typeIdx: map[string][]int{},
		dirs:    map[string]bool{},
	}
	ix.parseDecls(files)
	ix.finish()
	return ix, nil
}

func collectGoFiles(root string) ([]string, error) {
	skip := map[string]bool{"node_modules": true, "vendor": true, "dist": true, "build": true}
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if skip[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func (ix *index) parseDecls(files []string) {
	workers := workerCount()
	var mu sync.Mutex
	var wg sync.WaitGroup
	ch := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				rel := ix.rel(files[i])
				fact := fileFact{rel: rel}
				file, err := parser.ParseFile(ix.fset, files[i], nil, parser.ParseComments)
				if err == nil && file != nil {
					fact.pkg = file.Name.Name
				}
				mu.Lock()
				ix.files[i] = fact
				if err == nil && file != nil {
					ix.addDecls(file, fact)
				}
				mu.Unlock()
			}
		}()
	}
	for i := range files {
		ch <- i
	}
	close(ch)
	wg.Wait()
}

func aliasOf(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	path := strings.Trim(spec.Path.Value, `"`)
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		path = path[idx+1:]
	}
	return path
}

func workerCount() int {
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (ix *index) addDecls(file *ast.File, fact fileFact) {
	for _, d := range file.Decls {
		switch dd := d.(type) {
		case *ast.FuncDecl:
			decl := Decl{
				Name:      dd.Name.Name,
				Kind:      "func",
				Package:   fact.pkg,
				Signature: ix.exprText(dd.Type),
				Doc:       firstLine(dd.Doc),
				Range:     Range{File: fact.rel, Start: ix.line(dd.Pos()), End: ix.line(dd.End())},
			}
			if dd.Recv != nil && len(dd.Recv.List) > 0 {
				decl.Kind = "method"
				decl.Receiver = ix.receiverText(dd.Recv.List[0].Type)
			}
			ix.decls = append(ix.decls, decl)
		case *ast.GenDecl:
			for _, spec := range dd.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					decl := Decl{
						Name:      s.Name.Name,
						Kind:      "type",
						Package:   fact.pkg,
						Doc:       firstLine(specDoc(dd, s.Doc, s.Comment)),
						Signature: ix.exprText(s.Type),
						Range:     Range{File: fact.rel, Start: ix.line(s.Pos()), End: ix.line(s.End())},
					}
					switch t := s.Type.(type) {
					case *ast.StructType:
						decl.Kind = "struct"
						decl.Fields = fieldCount(t)
						ix.addFields(t, s.Name.Name, fact)
					case *ast.InterfaceType:
						decl.Kind = "interface"
						var names []string
						for _, m := range t.Methods.List {
							for _, n := range m.Names {
								names = append(names, n.Name)
							}
						}
						sort.Strings(names)
						decl.Methods = names
					}
					ix.decls = append(ix.decls, decl)
				case *ast.ValueSpec:
					kind := "var"
					if dd.Tok == token.CONST {
						kind = "const"
					}
					for _, n := range s.Names {
						ix.decls = append(ix.decls, Decl{
							Name:    n.Name,
							Kind:    kind,
							Package: fact.pkg,
							Doc:     firstLine(specDoc(dd, s.Doc, s.Comment)),
							Range:   Range{File: fact.rel, Start: ix.line(n.Pos()), End: ix.line(n.Pos())},
						})
					}
				}
			}
		}
	}
}

// addFields indexes the direct fields of a named struct; embedded fields are named after their type.
func (ix *index) addFields(st *ast.StructType, owner string, fact fileFact) {
	for _, f := range st.Fields.List {
		var names []string
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
		if len(names) == 0 {
			if name := embeddedName(f.Type); name != "" {
				names = append(names, name)
			}
		}
		for _, name := range names {
			ix.decls = append(ix.decls, Decl{
				Name:      name,
				Kind:      "field",
				Package:   fact.pkg,
				Receiver:  owner,
				Signature: ix.exprText(f.Type),
				Doc:       firstLine(firstDoc(f.Doc, f.Comment)),
				Range:     Range{File: fact.rel, Start: ix.line(f.Pos()), End: ix.line(f.End())},
			})
		}
	}
}

func embeddedName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return embeddedName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return embeddedName(t.X)
	case *ast.IndexListExpr:
		return embeddedName(t.X)
	}
	return ""
}

func firstDoc(docs ...*ast.CommentGroup) *ast.CommentGroup {
	for _, doc := range docs {
		if doc != nil {
			return doc
		}
	}
	return nil
}

func specDoc(gen *ast.GenDecl, docs ...*ast.CommentGroup) *ast.CommentGroup {
	for _, doc := range docs {
		if doc != nil {
			return doc
		}
	}
	return gen.Doc
}

func (ix *index) finish() {
	sort.SliceStable(ix.decls, func(i, j int) bool {
		a, b := ix.decls[i], ix.decls[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.Name < b.Name
	})
	for _, f := range ix.files {
		ix.dirs[dirOf(f.rel)] = true
	}
	for i, d := range ix.decls {
		ix.byName[d.Name] = append(ix.byName[d.Name], i)
		if isTypeKind(d.Kind) {
			ix.typeIdx[d.Name] = append(ix.typeIdx[d.Name], i)
		}
	}
}

func declKey(d Decl) string { return fmt.Sprintf("%s#%d#%s", d.File, d.Start, d.Name) }

func isTypeKind(kind string) bool {
	return kind == "type" || kind == "struct" || kind == "interface"
}

func baseReceiver(text string) string {
	text = strings.TrimPrefix(strings.TrimSpace(text), "*")
	if idx := strings.Index(text, "["); idx >= 0 {
		text = text[:idx]
	}
	return text
}

func (ix *index) line(pos token.Pos) int { return ix.fset.Position(pos).Line }

func (ix *index) receiverText(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + ix.receiverText(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return ix.receiverText(t.X)
	case *ast.IndexListExpr:
		return ix.receiverText(t.X)
	}
	return ix.exprText(expr)
}

func (ix *index) exprText(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	var b strings.Builder
	if err := printer.Fprint(&b, ix.fset, expr); err != nil {
		return ""
	}
	text := strings.Join(strings.Fields(b.String()), " ")
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return text
}

func firstLine(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	for _, line := range strings.Split(doc.Text(), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 160 {
				line = line[:160] + "..."
			}
			return line
		}
	}
	return ""
}

func fieldCount(t *ast.StructType) int {
	if t.Fields == nil {
		return 0
	}
	return len(t.Fields.List)
}

func (ix *index) rel(path string) string {
	r, err := filepath.Rel(ix.root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(r)
}

// dirOf is the repo-relative package directory of a file.
func dirOf(rel string) string {
	dir := path.Dir(rel)
	if dir == "." {
		return ""
	}
	return dir
}

// importDir maps an import path (jad/backend-go/internal/x or a full module URL) onto a package
// directory this index has seen, by longest matching suffix. It is how "pkg.Symbol" stops being a guess.
func (ix *index) importDir(importPath string) (string, bool) {
	if importPath == "" {
		return "", false
	}
	parts := strings.Split(strings.Trim(importPath, "/"), "/")
	for k := len(parts); k > 0; k-- {
		candidate := strings.Join(parts[len(parts)-k:], "/")
		if ix.dirs[candidate] {
			return candidate, true
		}
	}
	return "", false
}

// ---------- index (pass 2: references, only for one query) ----------

type target struct {
	name      string
	defs      []Range
	declSet   map[string]bool
	typeSet   map[string]bool
	refs      bool // a type, const, var or field is also used by name, not only called
	defLine   map[string]bool
	fieldOnly bool
}

// hit is one raw call site. Resolution happens afterwards because it needs the calling file's
// imports and directories, which are only known once every file has been seen.
type hit struct {
	name      string
	kind      string
	qualifier string
	file      string
	importsAt map[string]string
	rng       Range
	in        string
	line      int
	hints     []string
}

type scanned struct {
	hits    []hit
	callees []hit
	types   []string
	perFile map[string]int
}

func (ix *index) scanRefs(cfg config, tg target) scanned {
	out := scanned{perFile: map[string]int{}}
	abs := ix.scanDir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(ix.root, abs)
	}
	files, err := collectGoFiles(abs)
	if err != nil {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	ch := make(chan string)
	for w := 0; w < workerCount(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range ch {
				rel := ix.rel(path)
				if cfg.noTests && strings.HasSuffix(rel, "_test.go") {
					continue
				}
				fset := token.NewFileSet()
				file, err := parser.ParseFile(fset, path, nil, 0)
				if err != nil || file == nil {
					continue
				}
				hits, callees, types, mentions := scanFile(fset, file, rel, tg)
				mu.Lock()
				out.hits = append(out.hits, hits...)
				out.callees = append(out.callees, callees...)
				out.types = append(out.types, types...)
				out.perFile[rel] += mentions
				mu.Unlock()
			}
		}()
	}
	for _, path := range files {
		ch <- path
	}
	close(ch)
	wg.Wait()
	return out
}

// scanFile walks one file keeping the enclosing function, so every hit already has an address to read.
func scanFile(fset *token.FileSet, file *ast.File, rel string, tg target) (hits, callees []hit, types []string, mentions int) {
	importsAt := map[string]string{}
	for _, spec := range file.Imports {
		importsAt[aliasOf(spec)] = strings.Trim(spec.Path.Value, `"`)
	}
	line := func(p token.Pos) int { return fset.Position(p).Line }
	isTarget := func(r Range) bool {
		for _, d := range tg.defs {
			if d.File == r.File && d.Start == r.Start && d.End == r.End {
				return true
			}
		}
		return false
	}
	walk := func(node ast.Node, fn string, fnRange Range, inTarget bool) {
		// Type hints come from type positions only (params, fields, var types, composite literals), so a
		// value that happens to share a type's name never pulls an unrelated method into "callees".
		hints := map[string]bool{}
		ast.Inspect(node, func(n ast.Node) bool {
			var expr ast.Expr
			switch t := n.(type) {
			case *ast.CompositeLit:
				expr = t.Type
			case *ast.ValueSpec:
				expr = t.Type
			case *ast.Field:
				expr = t.Type
			case *ast.TypeAssertExpr:
				expr = t.Type
			case *ast.ArrayType:
				expr = t.Elt
			case *ast.MapType:
				collectTypeNames(t.Key, tg.typeSet, hints)
				expr = t.Value
			case *ast.ChanType:
				expr = t.Value
			}
			if expr != nil {
				collectTypeNames(expr, tg.typeSet, hints)
			}
			return true
		})
		if inTarget {
			for name := range hints {
				if name != tg.name && len(types) < 128 {
					types = append(types, name)
				}
			}
		}
		hintList := make([]string, 0, len(hints))
		for name := range hints {
			hintList = append(hintList, name)
		}
		called := map[token.Pos]bool{}
		selIdents := map[token.Pos]bool{}
		keyIdents := map[token.Pos]bool{}
		ast.Inspect(node, func(n ast.Node) bool {
			switch t := n.(type) {
			case *ast.CallExpr:
				called[t.Fun.Pos()] = true
				name, qual, kind := callTarget(t.Fun)
				if name == "" {
					return true
				}
				if inTarget {
					if tg.declSet[name] && name != tg.name {
						callees = append(callees, hit{name: name, kind: kind, qualifier: qual, file: rel, importsAt: importsAt, rng: fnRange, in: fn, line: line(t.Pos()), hints: hintList})
					}
					return true
				}
				if name == tg.name {
					hits = append(hits, hit{name: name, kind: kind, qualifier: qual, file: rel, importsAt: importsAt, rng: fnRange, in: fn, line: line(t.Pos())})
				}
			case *ast.KeyValueExpr:
				if id, ok := t.Key.(*ast.Ident); ok {
					keyIdents[id.Pos()] = true
				}
			case *ast.Ident:
				if t.Name != tg.name {
					return true
				}
				mentions++
				if tg.fieldOnly && !keyIdents[t.Pos()] {
					return true
				}
				if !inTarget && tg.refs && !called[t.Pos()] && !selIdents[t.Pos()] && !tg.defLine[fmt.Sprintf("%s#%d", rel, line(t.Pos()))] {
					hits = append(hits, hit{name: t.Name, kind: "ref", file: rel, importsAt: importsAt, rng: fnRange, in: fn, line: line(t.Pos())})
				}
			case *ast.SelectorExpr:
				selIdents[t.Sel.Pos()] = true
				if t.Sel.Name != tg.name || inTarget || !tg.refs || called[t.Pos()] || tg.defLine[fmt.Sprintf("%s#%d", rel, line(t.Sel.Pos()))] {
					return true
				}
				qual := ""
				if id, ok := t.X.(*ast.Ident); ok {
					qual = id.Name
				}
				hits = append(hits, hit{name: t.Sel.Name, kind: "selector_ref", qualifier: qual, file: rel, importsAt: importsAt, rng: fnRange, in: fn, line: line(t.Sel.Pos())})
			}
			return true
		})
	}
	for _, d := range file.Decls {
		switch dd := d.(type) {
		case *ast.FuncDecl:
			name := dd.Name.Name
			if dd.Recv != nil && len(dd.Recv.List) > 0 {
				name = baseReceiver(recvText(dd.Recv.List[0].Type)) + "." + name
			}
			r := Range{File: rel, Start: line(dd.Pos()), End: line(dd.End())}
			walk(dd, name, r, isTarget(r))
		case *ast.GenDecl:
			r := Range{File: rel, Start: line(dd.Pos()), End: line(dd.End())}
			walk(dd, "", r, isTarget(r))
		}
	}
	return hits, callees, types, mentions
}

// collectTypeNames keeps the names in one type expression that the repo declares as types.
func collectTypeNames(expr ast.Expr, typeSet map[string]bool, into map[string]bool) {
	if expr == nil {
		return
	}
	ast.Inspect(expr, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.Ident:
			if typeSet[t.Name] {
				into[t.Name] = true
			}
		case *ast.SelectorExpr:
			if typeSet[t.Sel.Name] {
				into[t.Sel.Name] = true
			}
		}
		return true
	})
}

// resolveHits keeps only hits that can reach a definition of the symbol, and says how it knows:
// same_package (unqualified call in a package that declares it), import (pkg-qualified call resolved
// through the file's imports) or name_only (call on a value; no type checker, so the name is all there is).
func (ix *index) resolveHits(hits []hit, defDirs map[string]bool, isMethod, isField bool) []Site {
	var out []Site
	for _, h := range hits {
		confidence := ""
		switch h.kind {
		case "call", "ref":
			if defDirs[dirOf(h.file)] {
				confidence = "same_package"
			}
		case "selector_call", "selector_ref":
			if dir, ok := ix.importDir(h.importsAt[h.qualifier]); ok {
				if defDirs[dir] {
					confidence = "import"
				}
			} else if h.kind == "selector_call" && isMethod {
				confidence = "name_only"
			} else if isField && (h.kind == "selector_call" || h.kind == "selector_ref") && (isExported(h.name) || defDirs[dirOf(h.file)]) {
				confidence = "name_only"
			}
		}
		if confidence == "" {
			continue
		}
		out = append(out, Site{In: h.in, Kind: h.kind, Qualifier: h.qualifier, Confidence: confidence, CallLine: h.line, Range: h.rng})
	}
	return dedupeSites(out)
}

// dedupeSites drops repeats of the same call in the same enclosing function (a function that calls the
// symbol three times is one place to read, listed once).
func dedupeSites(in []Site) []Site {
	seen := map[string]bool{}
	out := make([]Site, 0, len(in))
	for _, s := range in {
		key := fmt.Sprintf("%s#%d", s.File, s.CallLine)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	sortSites(out)
	return out
}

// resolveCallees turns the calls inside a definition into declarations, using the same directory rules
// and, for method calls on a value, the types the definition actually mentions.
func (ix *index) resolveCallees(hits []hit) []Decl {
	seen := map[string]bool{}
	var out []Decl
	add := func(d Decl) {
		key := declKey(d)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, d)
	}
	for _, h := range hits {
		switch h.kind {
		case "call":
			dir := dirOf(h.file)
			for _, id := range ix.byName[h.name] {
				if dirOf(ix.decls[id].File) == dir && ix.decls[id].Kind != "field" {
					add(ix.decls[id])
				}
			}
		case "selector_call":
			if dir, ok := ix.importDir(h.importsAt[h.qualifier]); ok {
				for _, id := range ix.byName[h.name] {
					if dirOf(ix.decls[id].File) == dir && ix.decls[id].Kind != "field" {
						add(ix.decls[id])
					}
				}
				continue
			}
			hint := map[string]bool{}
			for _, t := range h.hints {
				hint[t] = true
			}
			var candidates []Decl
			for _, id := range ix.byName[h.name] {
				if d := ix.decls[id]; d.Kind == "method" {
					candidates = append(candidates, d)
				}
			}
			if len(candidates) == 1 {
				add(candidates[0])
				continue
			}
			for _, d := range candidates {
				if hint[baseReceiver(d.Receiver)] {
					add(d)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Start < out[j].Start
	})
	return out
}

func callTarget(fun ast.Expr) (name, qualifier, kind string) {
	switch t := fun.(type) {
	case *ast.Ident:
		return t.Name, "", "call"
	case *ast.SelectorExpr:
		qual := ""
		if id, ok := t.X.(*ast.Ident); ok {
			qual = id.Name
		}
		return t.Sel.Name, qual, "selector_call"
	case *ast.IndexExpr:
		return callTarget(t.X)
	case *ast.IndexListExpr:
		return callTarget(t.X)
	case *ast.ParenExpr:
		return callTarget(t.X)
	}
	return "", "", ""
}

func recvText(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + recvText(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvText(t.X)
	case *ast.IndexListExpr:
		return recvText(t.X)
	}
	return ""
}

func sortSites(sites []Site) {
	sort.SliceStable(sites, func(i, j int) bool {
		a, b := sites[i], sites[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.CallLine != b.CallLine {
			return a.CallLine < b.CallLine
		}
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.In < b.In
	})
}

// ---------- commands ----------

// symAnswer is one symbol's typed result; JSON and text are both rendered from it.
type symAnswer struct {
	cmd        string
	symbol     string
	failure    map[string]any
	defs       []Decl
	callers    []Site
	tests      []Site
	callees    []Decl
	related    []Decl
	files      []Site
	counts     map[string]int
	truncated  map[string]bool
	totals     map[string]int
	wantsRefs  bool
	candidates []candidate
}

type candidate struct {
	Key     string `json:"key"`
	Defs    []Decl `json:"definitions"`
	Callers []Site `json:"callers,omitempty"`
	Tests   []Site `json:"tests,omitempty"`
}

type session struct {
	cfg     config
	ix      *index
	started time.Time
	answers []*symAnswer
}

func splitSymbols(arg string) []string {
	return strings.FieldsFunc(arg, func(r rune) bool { return r == '|' || r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

func isExported(name string) bool { return name != "" && name[0] >= 'A' && name[0] <= 'Z' }

func inTestFile(file string) bool { return strings.HasSuffix(file, "_test.go") }

func symbolList(cfg config) []string {
	if len(cfg.symbols) > 0 {
		return cfg.symbols
	}
	if cfg.symbol != "" {
		return splitSymbols(cfg.symbol)
	}
	return nil
}

func openIndex(cfg *config) (*index, error) {
	if cfg.path == "" {
		cfg.path = "backend-go"
		if info, err := os.Stat(filepath.Join(cfg.root, cfg.path)); err != nil || !info.IsDir() {
			cfg.path = "."
		}
	}
	return newIndex(cfg.root, cfg.path)
}

// lookup resolves Name, Type.Member, pkg.Name or pkg.Type.Member to declaration indices. A package
// qualifier that names a package prefers that package's own declarations, so pkg.Name and Type.Method
// never collide; a receiver qualifier picks the type's methods and fields.
func (ix *index) lookup(spec string, cfg config) []int {
	name, qual := spec, ""
	if i := strings.LastIndex(spec, "."); i > 0 {
		qual, name = spec[:i], spec[i+1:]
	}
	var loose []int
	for _, id := range ix.byName[name] {
		d := ix.decls[id]
		if cfg.noTests && inTestFile(d.File) {
			continue
		}
		recv := baseReceiver(d.Receiver)
		if cfg.receiver != "" && recv != "" && recv != cfg.receiver {
			continue
		}
		if qual != "" {
			own := qual == d.Package && recv == ""
			byType := recv != "" && (qual == recv || qual == d.Package+"."+recv)
			if !own {
				if byType {
					loose = append(loose, id)
				}
				continue
			}
		}
		loose = append(loose, id)
	}
	return loose
}

type group struct {
	key   string
	dir   string
	defs  []Decl
	dirs  map[string]bool
	isMth bool
	isFld bool
}

// groupDefs splits definitions by (package directory, receiver): same-named symbols in different
// packages or on different types are different symbols.
func groupDefs(defs []Decl) []*group {
	byKey := map[string]*group{}
	var order []*group
	for _, d := range defs {
		k := dirOf(d.File) + "|" + baseReceiver(d.Receiver)
		g := byKey[k]
		if g == nil {
			g = &group{dir: dirOf(d.File), dirs: map[string]bool{dirOf(d.File): true}}
			byKey[k] = g
			order = append(order, g)
		}
		g.defs = append(g.defs, d)
		g.isMth = g.isMth || d.Kind == "method"
		g.isFld = g.isFld || d.Kind == "field"
	}
	count := map[string]int{}
	for _, g := range order {
		count[displayKey(g.defs[0], false)]++
	}
	for _, g := range order {
		g.key = displayKey(g.defs[0], count[displayKey(g.defs[0], false)] > 1)
	}
	return order
}

func displayKey(d Decl, withPkg bool) string {
	if r := baseReceiver(d.Receiver); r != "" {
		if withPkg {
			return d.Package + "." + r + "." + d.Name
		}
		return r + "." + d.Name
	}
	return d.Package + "." + d.Name
}

func wantsRefsKind(kind string) bool {
	return isTypeKind(kind) || kind == "const" || kind == "var" || kind == "field"
}

// openSession parses nothing twice: it normalises the command, checks that a symbol was given and
// builds the engine A index. Both the fast lookups and engine B start here.
func openSession(cfg config, started time.Time) (*session, []string, error) {
	cmd := cfg.cmd
	if cmd == "" {
		cmd = "navigate"
	}
	cfg.cmd = cmd
	specs := symbolList(cfg)
	if len(specs) == 0 {
		return nil, nil, usagef("no symbol given; usage: gonav %s SYMBOL [SYMBOL...]", cmd)
	}
	ix, err := openIndex(&cfg)
	if err != nil {
		return nil, nil, err
	}
	return &session{cfg: cfg, ix: ix, started: started}, specs, nil
}

func buildSession(cfg config, started time.Time) (*session, error) {
	se, specs, err := openSession(cfg, started)
	if err != nil {
		return nil, err
	}
	for _, spec := range specs {
		se.answers = append(se.answers, se.resolve(spec))
	}
	return se, nil
}

func (se *session) resolve(spec string) *symAnswer {
	cfg, ix, cmd := se.cfg, se.ix, se.cfg.cmd
	ans := &symAnswer{cmd: cmd, symbol: spec, counts: map[string]int{}, truncated: map[string]bool{}}
	ids := ix.lookup(spec, cfg)
	if len(ids) == 0 {
		name := spec
		if i := strings.LastIndex(spec, "."); i > 0 {
			name = spec[i+1:]
		}
		ans.failure = map[string]any{
			"ok":          false,
			"cmd":         cmd,
			"symbol":      spec,
			"error":       "no declaration named " + spec,
			"scan":        map[string]any{"path": cfg.path, "files": len(ix.files)},
			"suggestions": ix.suggest(name),
		}
		return ans
	}
	for _, id := range ids {
		ans.defs = append(ans.defs, ix.decls[id])
	}
	groups := groupDefs(ans.defs)
	if len(groups) > 1 {
		for _, g := range groups {
			ans.candidates = append(ans.candidates, candidate{Key: g.key, Defs: g.defs})
		}
	}
	if cmd == "symbol" || cmd == "show" {
		return ans
	}
	tg := target{
		name:    spec,
		defLine: map[string]bool{},
		declSet: keysOf(ix.byName),
		typeSet: keysOf(ix.typeIdx),
	}
	if i := strings.LastIndex(spec, "."); i > 0 {
		tg.name = spec[i+1:]
	}
	tg.fieldOnly = true
	for _, d := range ans.defs {
		tg.defs = append(tg.defs, d.Range)
		if wantsRefsKind(d.Kind) {
			tg.refs = true
			ans.wantsRefs = true
		}
		if d.Kind != "field" {
			tg.fieldOnly = false
		}
		if d.Kind == "const" || d.Kind == "var" || d.Kind == "field" {
			tg.defLine[fmt.Sprintf("%s#%d", d.File, d.Start)] = true
		}
	}
	sc := ix.scanRefs(cfg, tg)
	split := func(g *group) (callers, tests []Site) {
		for _, site := range ix.resolveHits(sc.hits, g.dirs, g.isMth, g.isFld) {
			if inTestFile(site.File) {
				tests = append(tests, site)
			} else {
				callers = append(callers, site)
			}
		}
		return
	}
	if len(groups) > 1 {
		for i, g := range groups {
			c, t := split(g)
			ans.candidates[i].Callers, ans.candidates[i].Tests = c, t
		}
		return ans
	}
	callers, tests := split(groups[0])
	if !cfg.noTests {
		tests = append(tests, ix.testDecls(tg.name)...)
		sortSites(tests)
	}
	ans.callers, ans.tests = callers, tests
	ans.callees = ix.resolveCallees(sc.callees)
	ans.related = dedupeDecls(ix.resolveTypeDecls(unique(sc.types)))
	if len(ans.defs) == 1 && ans.defs[0].Kind == "interface" {
		ans.related = dedupeDecls(append(ans.related, ix.implementers(ans.defs[0])...))
	}
	files := make([]Site, 0, len(sc.perFile))
	for file, n := range sc.perFile {
		files = append(files, Site{Kind: "file", Mentions: n, Range: Range{File: file}})
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Mentions != files[j].Mentions {
			return files[i].Mentions > files[j].Mentions
		}
		return files[i].File < files[j].File
	})
	ans.files = files
	ans.counts = map[string]int{
		"definitions": len(ans.defs),
		"callers":     len(callers),
		"callees":     len(ans.callees),
		"types":       len(ans.related),
		"tests":       len(tests),
		"files":       len(sc.perFile),
		"mentions":    totalMentions(sc.perFile),
	}
	return ans
}

func (se *session) scanInfo() map[string]any {
	return map[string]any{"path": se.cfg.path, "files": len(se.ix.files), "engine": "go/ast", "ms": time.Since(se.started).Milliseconds()}
}

func (se *session) toJSON(ans *symAnswer) map[string]any {
	cfg, cmd := se.cfg, ans.cmd
	if ans.failure != nil {
		return ans.failure
	}
	if len(ans.candidates) > 0 {
		return map[string]any{
			"ok":          false,
			"cmd":         cmd,
			"symbol":      ans.symbol,
			"ambiguous":   true,
			"definitions": ans.defs,
			"count":       len(ans.defs),
			"candidates":  ans.candidates,
			"ranges":      rangesOf(ans.defs),
			"note":        "several definitions share this name; pass pkg.Name or Type.Method to pick one",
			"scan":        se.scanInfo(),
		}
	}
	if cmd == "symbol" {
		return map[string]any{
			"ok":          true,
			"cmd":         cmd,
			"symbol":      ans.symbol,
			"definitions": ans.defs,
			"count":       len(ans.defs),
			"ranges":      rangesOf(ans.defs),
			"scan":        se.scanInfo(),
		}
	}
	result := map[string]any{
		"ok":          true,
		"cmd":         cmd,
		"symbol":      ans.symbol,
		"definitions": ans.defs,
		"counts":      ans.counts,
		"scan":        se.scanInfo(),
	}
	truncated := map[string]bool{}
	switch cmd {
	case "navigate":
		result["callers"] = bound(cfg.max, ans.callers, truncated, "callers")
		result["callees"] = boundDecls(cfg.max, ans.callees, truncated, "callees")
		result["types"] = boundDecls(cfg.max, ans.related, truncated, "types")
		result["tests"] = bound(cfg.max, ans.tests, truncated, "tests")
		result["ranges"] = chainRanges(ans.defs, ans.callers, ans.tests)
		if ans.wantsRefs {
			result["note"] = "callers are the places that name it (ref) or call through it (call)"
		}
	case "impact":
		result["files"] = bound(cfg.max, ans.files, truncated, "files")
		result["callers"] = bound(cfg.max, ans.callers, truncated, "callers")
		result["tests"] = bound(cfg.max, ans.tests, truncated, "tests")
		result["ranges"] = chainRanges(ans.defs, ans.callers, ans.tests)
	}
	if _, set := result["note"]; !set {
		result["note"] = "callers are call sites with their resolution (same_package, import or name_only); callees come from the definition body; no type checker runs, so verify a doubtful hit with its range."
	}
	if len(truncated) > 0 {
		result["truncated"] = truncated
	}
	return result
}

func (se *session) jsonResult() (any, error) {
	if len(se.answers) == 1 {
		ans := se.answers[0]
		out := se.toJSON(ans)
		if ans.failure != nil || len(ans.candidates) > 0 {
			return nil, &resultError{value: out}
		}
		return out, nil
	}
	results := make([]map[string]any, 0, len(se.answers))
	failed := 0
	for _, ans := range se.answers {
		if ans.failure != nil || len(ans.candidates) > 0 {
			failed++
		}
		results = append(results, se.toJSON(ans))
	}
	out := map[string]any{"ok": failed < len(se.answers), "cmd": se.cfg.cmd, "results": results}
	if failed == len(se.answers) {
		return nil, &resultError{value: out}
	}
	return out, nil
}

func symbolResult(cfg config, started time.Time) (any, error) {
	se, err := buildSession(cfg, started)
	if err != nil {
		return nil, err
	}
	return se.jsonResult()
}

func totalMentions(perFile map[string]int) int {
	total := 0
	for _, n := range perFile {
		total += n
	}
	return total
}

func rangesOf(decls []Decl) []Range {
	out := make([]Range, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.Range)
	}
	return out
}

func keysOf(m map[string][]int) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func unique(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (ix *index) resolveDecls(names []string) []Decl {
	var out []Decl
	for _, name := range names {
		for _, id := range ix.byName[name] {
			out = append(out, ix.decls[id])
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Start < out[j].Start
	})
	return out
}

// resolveTypeDecls resolves names to type declarations only, never to same-named funcs or methods.
func (ix *index) resolveTypeDecls(names []string) []Decl {
	var out []Decl
	for _, name := range names {
		for _, id := range ix.typeIdx[name] {
			out = append(out, ix.decls[id])
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Start < out[j].Start
	})
	return out
}

func dedupeDecls(in []Decl) []Decl {
	seen := map[string]bool{}
	var out []Decl
	for _, d := range in {
		key := declKey(d)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// implementers finds named types whose method set covers the interface, plus embedded-free cases only.
func (ix *index) implementers(iface Decl) []Decl {
	if len(iface.Methods) == 0 {
		return nil
	}
	if len(ix.decls) == 0 {
		return nil
	}
	byType := map[string][]Decl{}
	for _, d := range ix.decls {
		if d.Kind != "method" {
			continue
		}
		base := baseReceiver(d.Receiver)
		byType[base] = append(byType[base], d)
	}
	var out []Decl
	for typeName, methods := range byType {
		have := map[string]bool{}
		for _, m := range methods {
			have[m.Name] = true
		}
		ok := true
		for _, want := range iface.Methods {
			if !have[want] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		for _, id := range ix.typeIdx[typeName] {
			out = append(out, ix.decls[id])
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Start < out[j].Start
	})
	if len(out) > maxCallersPer {
		out = out[:maxCallersPer]
	}
	return out
}

// testDecls are Test/Benchmark/Example functions that name the symbol as a CamelCase word.
func (ix *index) testDecls(symbol string) []Site {
	var out []Site
	forms := []string{symbol}
	if symbol != "" && isLowerByte(symbol[0]) {
		// A lowercase symbol only names a test in its exported form: "sync" must not match TestAsync.
		forms = []string{upperFirst(symbol)}
	}
	for _, d := range ix.decls {
		if !strings.HasSuffix(d.File, "_test.go") || d.Kind != "func" {
			continue
		}
		if !hasTestPrefix(d.Name) {
			continue
		}
		if camelWord(d.Name, forms) {
			out = append(out, Site{In: d.Name, Kind: "test_func", Range: d.Range})
		}
	}
	return out
}

func hasTestPrefix(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Example"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// camelWord reports whether name contains one of forms as a word: the character after the match must
// not continue a lowercase word, so "Sync" matches TestSyncOrder but "sync" never matches TestAsync.
func camelWord(name string, forms []string) bool {
	for _, form := range forms {
		for at := 0; at < len(name); {
			idx := strings.Index(name[at:], form)
			if idx < 0 {
				break
			}
			idx += at
			end := idx + len(form)
			if end == len(name) || !isLowerByte(name[end]) {
				return true
			}
			at = idx + 1
		}
	}
	return false
}

func isLowerByte(b byte) bool { return b >= 'a' && b <= 'z' }

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	first := s[0]
	if first >= 'a' && first <= 'z' {
		return string(first-'a'+'A') + s[1:]
	}
	return s
}

func withoutTestFiles(sites []Site) []Site {
	var out []Site
	for _, s := range sites {
		if !strings.HasSuffix(s.File, "_test.go") {
			out = append(out, s)
		}
	}
	return out
}

func bound(max int, sites []Site, truncated map[string]bool, section string) []Site {
	if len(sites) <= max {
		return sites
	}
	truncated[section] = true
	return sites[:max]
}

func boundDecls(max int, decls []Decl, truncated map[string]bool, section string) []Decl {
	if len(decls) <= max {
		return decls
	}
	truncated[section] = true
	return decls[:max]
}

func chainRanges(defs []Decl, callers, tests []Site) []Range {
	seen := map[string]bool{}
	var out []Range
	add := func(r Range) {
		key := fmt.Sprintf("%s#%d#%d", r.File, r.Start, r.End)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, r)
	}
	for _, d := range defs {
		add(d.Range)
	}
	for _, s := range callers {
		add(Range{File: s.File, Start: s.Start, End: s.End})
	}
	for _, s := range tests {
		add(Range{File: s.File, Start: s.Start, End: s.End})
	}
	return out
}

// suggest offers the declared names closest to a miss: substring matches first, then near misses
// (a typo, a missing letter), so a wrong guess still leads somewhere.
func (ix *index) suggest(name string) []string {
	lower := strings.ToLower(name)
	type scored struct {
		name     string
		distance int
	}
	var out []scored
	for candidate := range ix.byName {
		c := strings.ToLower(candidate)
		switch {
		case strings.Contains(c, lower) || strings.Contains(lower, c):
			// A substring hit is a wider guess than a one-letter typo, so it ranks after one.
			out = append(out, scored{candidate, 2})
		default:
			if d := editDistance(lower, c, 2); d <= 2 {
				out = append(out, scored{candidate, d})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].distance != out[j].distance {
			return out[i].distance < out[j].distance
		}
		if len(out[i].name) != len(out[j].name) {
			return len(out[i].name) < len(out[j].name)
		}
		return out[i].name < out[j].name
	})
	names := make([]string, 0, maxSuggestions)
	for _, item := range out {
		if len(names) == maxSuggestions {
			break
		}
		names = append(names, item.name)
	}
	return names
}

// editDistance is a bounded Levenshtein distance: it stops once the best possible value exceeds limit.
func editDistance(a, b string, limit int) int {
	if a == b {
		return 0
	}
	if abs(len(a)-len(b)) > limit {
		return limit + 1
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		best := current[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min3(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
			if current[j] < best {
				best = current[j]
			}
		}
		if best > limit {
			return limit + 1
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

type srcBlock struct {
	rng   Range
	err   string
	lines []string
}

// readBlocks reads the ranges until maxLines is reached; it reports the lines it has not shown.
func readBlocks(cfg config, ranges []Range) (blocks []srcBlock, shown, omitted int) {
	for _, r := range ranges {
		path := r.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(cfg.root, r.File)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			blocks = append(blocks, srcBlock{rng: r, err: fmt.Sprintf("%s: %v", r.File, err)})
			continue
		}
		src := strings.Split(string(data), "\n")
		start, end := r.Start, r.End
		if start < 1 {
			start = 1
		}
		if end == 0 || end > len(src) {
			end = len(src)
		}
		blk := srcBlock{rng: Range{File: r.File, Start: start, End: end}}
		for n := start; n <= end; n++ {
			if shown >= cfg.maxLines {
				omitted++
				continue
			}
			blk.lines = append(blk.lines, src[n-1])
			shown++
		}
		blocks = append(blocks, blk)
	}
	return
}

func sourceRanges(cfg config) ([]Range, error) {
	ranges := cfg.ranges
	if len(ranges) == 0 && cfg.file != "" {
		start, end := cfg.startLine, cfg.endLine
		if start == 0 {
			start = 1
		}
		if end == 0 {
			end = start
		}
		ranges = []Range{{File: cfg.file, Start: start, End: end}}
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf(`no ranges; pass {"ranges":[{"file":"a.go","start_line":2,"end_line":9}]} on stdin, or FILE [START [END]]`)
	}
	return ranges, nil
}

func sourceResult(cfg config, started time.Time) (map[string]any, error) {
	ranges, err := sourceRanges(cfg)
	if err != nil {
		return nil, err
	}
	return sourceJSON(cfg, started, ranges), nil
}

func sourceJSON(cfg config, started time.Time, ranges []Range) map[string]any {
	blocks, shown, omitted := readBlocks(cfg, ranges)
	var b strings.Builder
	var used []Range
	for _, blk := range blocks {
		if blk.err != "" {
			b.WriteString(blk.err + "\n")
			continue
		}
		if len(blk.lines) == 0 {
			continue
		}
		for i, l := range blk.lines {
			fmt.Fprintf(&b, "%s:%d\t%s\n", blk.rng.File, blk.rng.Start+i, l)
		}
		used = append(used, blk.rng)
	}
	return map[string]any{
		"ok":        true,
		"cmd":       "source",
		"lines":     shown,
		"ranges":    used,
		"truncated": omitted > 0,
		"ms":        time.Since(started).Milliseconds(),
		"text":      b.String(),
	}
}

func renderBlocks(blocks []srcBlock, omitted int) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.err != "" {
			b.WriteString(blk.err + "\n")
			continue
		}
		if len(blk.lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s:%d-%d\n", blk.rng.File, blk.rng.Start, blk.rng.Start+len(blk.lines)-1)
		for i, l := range blk.lines {
			fmt.Fprintf(&b, "%d\t%s\n", blk.rng.Start+i, l)
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "... truncated: %d more lines not shown; raise --max-lines\n", omitted)
	}
	return b.String()
}

// showRanges resolves each symbol to the ranges to print; ambiguous or missing symbols come back as notes.
func (se *session) showRanges() (ranges []Range, notes []string) {
	for _, ans := range se.answers {
		switch {
		case ans.failure != nil:
			notes = append(notes, renderError(ans.failure))
		case len(ans.candidates) > 0:
			var b strings.Builder
			fmt.Fprintf(&b, "ambiguous %s: %d candidates; pass one of the keys below\n", ans.symbol, len(ans.candidates))
			for _, c := range ans.candidates {
				for _, d := range c.Defs {
					fmt.Fprintf(&b, "%s:%d-%d\t%s\t%s\n", d.File, d.Start, d.End, d.Kind, c.Key)
				}
			}
			notes = append(notes, b.String())
		default:
			ranges = append(ranges, rangesOf(ans.defs)...)
		}
	}
	return
}

func showResult(cfg config, started time.Time) (map[string]any, error) {
	se, err := buildSession(cfg, started)
	if err != nil {
		return nil, err
	}
	ranges, notes := se.showRanges()
	out := sourceJSON(se.cfg, started, ranges)
	if len(notes) > 0 {
		out["notes"] = notes
	}
	out["cmd"] = "show"
	if len(ranges) == 0 && se.unresolved() > 0 {
		out["ok"] = false
		out["ambiguous"] = se.ambiguous()
		out["candidates"] = se.candidates()
		return nil, &resultError{value: out}
	}
	return out, nil
}

// unresolved counts the symbols that named no single definition: missing, or ambiguous.
func (se *session) unresolved() int {
	n := 0
	for _, ans := range se.answers {
		if ans.failure != nil || len(ans.candidates) > 0 {
			n++
		}
	}
	return n
}

// candidates lists the candidates of every ambiguous symbol, in symbol order.
func (se *session) candidates() []candidate {
	var out []candidate
	for _, ans := range se.answers {
		out = append(out, ans.candidates...)
	}
	return out
}

// ambiguous reports whether any symbol matched several definitions.
func (se *session) ambiguous() bool {
	for _, ans := range se.answers {
		if len(ans.candidates) > 0 {
			return true
		}
	}
	return false
}

// ---------- compact text output ----------

func executeText(cfg config, started time.Time) (string, error) {
	switch cfg.cmd {
	case "source":
		ranges, err := sourceRanges(cfg)
		if err != nil {
			return "", err
		}
		blocks, _, omitted := readBlocks(cfg, ranges)
		return capText(renderBlocks(blocks, omitted), cfg, ""), nil
	case "refs", "callers", "impl":
		return typedText(cfg, started)
	case "navigate", "symbol", "impact", "show", "":
		se, err := buildSession(cfg, started)
		if err != nil {
			return "", err
		}
		if se.cfg.cmd == "show" {
			ranges, notes := se.showRanges()
			blocks, _, omitted := readBlocks(se.cfg, ranges)
			text := capText(strings.Join(notes, "")+renderBlocks(blocks, omitted), se.cfg, "")
			if len(ranges) == 0 && se.unresolved() > 0 {
				return "", &resultError{value: map[string]any{"text": text}}
			}
			return text, nil
		}
		var b strings.Builder
		failed := 0
		for _, ans := range se.answers {
			if ans.failure != nil || len(ans.candidates) > 0 {
				failed++
			}
			b.WriteString(se.renderText(ans))
		}
		text := capText(b.String(), se.cfg, "narrow with pkg.Name or Type.Method, --no-tests, --path DIR or --max N")
		if failed == len(se.answers) {
			return "", &resultError{value: map[string]any{"text": text}}
		}
		return text, nil
	}
	return "", usagef("unknown command %q; use navigate, symbol, impact, show, refs, callers, impl or source", cfg.cmd)
}

func renderError(v map[string]any) string {
	if text, ok := v["text"].(string); ok {
		return text
	}
	var b strings.Builder
	fmt.Fprintf(&b, "error: %v\n", v["error"])
	if sg, ok := v["suggestions"].([]string); ok && len(sg) > 0 {
		fmt.Fprintf(&b, "suggestions: %s\n", strings.Join(sg, ", "))
	}
	return b.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func declLine(d Decl, key string, label ...string) string {
	kind := d.Kind
	if len(label) > 0 {
		kind = label[0] + ":" + d.Kind
	}
	detail := ""
	switch d.Kind {
	case "struct", "interface":
		if d.Fields > 0 {
			detail = fmt.Sprintf("fields=%d", d.Fields)
		}
		if len(d.Methods) > 0 {
			detail = "methods=" + strings.Join(d.Methods, ",")
		}
	default:
		detail = clip(strings.ReplaceAll(d.Signature, "\n", " "), 100)
	}
	return strings.TrimRight(fmt.Sprintf("%s:%d-%d\t%s\t%s\t%s", d.File, d.Start, d.End, kind, key, detail), "\t") + "\n"
}

func siteLine(label string, s Site) string {
	detail := s.In
	if s.CallLine > 0 {
		detail += fmt.Sprintf(" L%d", s.CallLine)
	}
	if s.Confidence != "" {
		detail += " " + s.Confidence
	}
	return fmt.Sprintf("%s:%d-%d\t%s\t%s\n", s.File, s.Start, s.End, label, strings.TrimSpace(detail))
}

func (se *session) renderText(ans *symAnswer) string {
	cfg := se.cfg
	if ans.failure != nil {
		return renderError(ans.failure)
	}
	var b strings.Builder
	if len(ans.candidates) > 0 {
		fmt.Fprintf(&b, "%s %s: ambiguous, %d candidates; pass one key (pkg.Name or Type.Method)\n", ans.cmd, ans.symbol, len(ans.candidates))
		for _, c := range ans.candidates {
			for _, d := range c.Defs {
				b.WriteString(declLine(d, c.Key))
			}
		}
		return b.String()
	}
	if ans.cmd == "symbol" {
		fmt.Fprintf(&b, "symbol %s: %d definitions\n", ans.symbol, len(ans.defs))
	} else {
		fmt.Fprintf(&b, "%s %s: defs=%d callers=%d callees=%d types=%d tests=%d files=%d mentions=%d\n", ans.cmd, ans.symbol,
			ans.counts["definitions"], ans.counts["callers"], ans.counts["callees"], ans.counts["types"], ans.counts["tests"], ans.counts["files"], ans.counts["mentions"])
	}
	for _, d := range ans.defs {
		b.WriteString(declLine(d, displayKey(d, false)))
	}
	if ans.cmd == "symbol" {
		return b.String()
	}
	section := func(label string, sites []Site) {
		shown := limitSites(sites, cfg.max)
		for _, s := range shown {
			b.WriteString(siteLine(label, s))
		}
		if len(shown) < len(sites) {
			fmt.Fprintf(&b, "... %s: showing %d of %d; use --max N\n", label, len(shown), len(sites))
		}
	}
	decls := func(label string, list []Decl) {
		for i, d := range list {
			if i >= cfg.max {
				fmt.Fprintf(&b, "... %s: showing %d of %d; use --max N\n", label, cfg.max, len(list))
				break
			}
			b.WriteString(declLine(d, displayKey(d, false), label))
		}
	}
	section("caller", ans.callers)
	if ans.cmd == "navigate" {
		decls("callee", ans.callees)
		decls("type", ans.related)
	}
	if ans.cmd == "impact" {
		for i, f := range ans.files {
			if i >= cfg.max {
				fmt.Fprintf(&b, "... files: showing %d of %d; use --max N\n", cfg.max, len(ans.files))
				break
			}
			fmt.Fprintf(&b, "%s\tfile\tmentions=%d\n", f.File, f.Mentions)
		}
	}
	section("test", ans.tests)
	return b.String()
}

func limitSites(sites []Site, max int) []Site {
	if len(sites) > max {
		return sites[:max]
	}
	return sites
}

// capText enforces the byte cap on whole lines and says exactly what was cut.
func capText(text string, cfg config, hint string) string {
	limit := cfg.maxBytes
	if limit == 0 {
		limit = textCapBytes
	}
	if len(text) <= limit {
		return text
	}
	totalLines := strings.Count(text, "\n")
	cut := strings.LastIndex(text[:limit], "\n") + 1
	shownLines := strings.Count(text[:cut], "\n")
	if hint == "" {
		hint = "raise --max-bytes, lower --max-lines or narrow the range"
	}
	return fmt.Sprintf("%s... truncated: showing %d of %d lines (%d of %d bytes); %s\n", text[:cut], shownLines, totalLines, cut, len(text), hint)
}

// ---------- engine B: go/types over go list export data ----------

// listPackage is the subset of "go list -json" this engine needs.
type listPackage struct {
	ImportPath      string
	Name            string
	Dir             string
	ForTest         string
	Standard        bool
	DepOnly         bool
	Export          string
	GoFiles         []string
	CompiledGoFiles []string
	IgnoredGoFiles  []string
	CgoFiles        []string
	Imports         []string
	ImportMap       map[string]string
	Error           *struct{ Err string }
	Module          *struct {
		Path string
		Dir  string
		Main bool
	}
}

// baseImportPath strips the " [x.test]" suffix go list puts on test variants.
func baseImportPath(p string) string {
	if i := strings.Index(p, " ["); i > 0 {
		return p[:i]
	}
	return p
}

func testVariant(importPath, suffix string) string {
	return baseImportPath(importPath) + " [" + baseImportPath(importPath) + ".test]" + suffix
}

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return "go"
}

func findModuleDir(start string) (string, bool) {
	dir := start
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// loader is one "go list -export -deps -json" result: every package (and, with tests, every test
// variant) the module compiles, with the export data path each has.
type loader struct {
	root      string
	dir       string
	path      string
	withTests bool
	pkgs      []*listPackage
	byPath    map[string]*listPackage
	module    []*listPackage
	base      map[string][]*listPackage
	dirPath   map[string]string
}

func loadModule(root, scanDir string, withTests bool) (*loader, error) {
	abs := scanDir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, scanDir)
	}
	dir, ok := findModuleDir(abs)
	if !ok {
		return nil, fmt.Errorf("no go.mod above %s", scanDir)
	}
	args := []string{"list", "-e", "-export", "-deps", "-json", "-compiled"}
	if withTests {
		args = append(args, "-test")
	}
	args = append(args, "./...")
	cmd := exec.Command(goTool(), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("go list in %s: %v: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	ld := &loader{root: root, dir: dir, withTests: withTests, byPath: map[string]*listPackage{}, base: map[string][]*listPackage{}, dirPath: map[string]string{}}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listPackage
		if err := dec.Decode(&p); err != nil {
			break
		}
		ld.pkgs = append(ld.pkgs, &p)
	}
	for _, p := range ld.pkgs {
		ld.byPath[p.ImportPath] = p
		if p.ImportPath == baseImportPath(p.ImportPath) {
			ld.base[p.ImportPath] = append(ld.base[p.ImportPath], p)
			if _, seen := ld.dirPath[p.Dir]; !seen {
				ld.dirPath[p.Dir] = p.ImportPath
			}
		}
		if p.Module != nil && p.Module.Main {
			ld.module = append(ld.module, p)
			ld.path = p.Module.Path
		}
	}
	if len(ld.pkgs) == 0 {
		return nil, fmt.Errorf("go list in %s returned no packages", dir)
	}
	return ld, nil
}

// variant finds one flavour of a package: "prod" is the plain build, "aug" the package plus its
// internal test files, "xtest" the external (x_test) test package, all as go list -test names them.
func (ld *loader) variant(base, kind string) *listPackage {
	switch kind {
	case "prod":
		return ld.byPath[base]
	case "aug":
		return ld.byPath[base+" ["+base+".test]"]
	case "xtest":
		return ld.byPath[base+"_test ["+base+".test]"]
	}
	return nil
}

// importersOf lists the module packages that import base, test variants included, so the type-check
// stays local instead of covering the whole module.
func (ld *loader) importersOf(base string) []*listPackage {
	var out []*listPackage
	for _, p := range ld.module {
		if baseImportPath(p.ImportPath) == base {
			continue
		}
		for _, dep := range p.Imports {
			if mapped, ok := p.ImportMap[dep]; ok {
				dep = mapped
			}
			if baseImportPath(dep) == base {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func (ld *loader) lookupExport(path string) (io.ReadCloser, error) {
	if lp := ld.byPath[path]; lp != nil && lp.Export != "" {
		return os.Open(lp.Export)
	}
	for _, lp := range ld.base[baseImportPath(path)] {
		if lp.Export != "" {
			return os.Open(lp.Export)
		}
	}
	return nil, fmt.Errorf("no export data for %q", path)
}

type checkedPkg struct {
	lp    *listPackage
	pkg   *types.Package
	info  *types.Info
	files []*ast.File
	model map[token.Pos]funcSpan
}

type funcSpan struct {
	start, end token.Pos
	name       string
}

type checker struct {
	ld       *loader
	fset     *token.FileSet
	gc       types.Importer
	packages map[string]*checkedPkg
	partial  bool
	problems []string
}

func newChecker(ld *loader) *checker {
	ck := &checker{ld: ld, fset: token.NewFileSet(), packages: map[string]*checkedPkg{}}
	ck.gc = importer.ForCompiler(ck.fset, "gc", ld.lookupExport)
	return ck
}

func (ck *checker) problem(err error) {
	ck.partial = true
	if len(ck.problems) < 8 {
		ck.problems = append(ck.problems, err.Error())
	}
}

// variantImporter resolves every import through export data, always to the plain build of the package.
// One path must mean one type identity for the whole run, so neither go list's test-variant remapping
// (ImportMap) nor an in-memory copy may be substituted: a package's test files never declare anything
// another package can import, and references are recognised by package, name and receiver.
type variantImporter struct{ ck *checker }

func (vi *variantImporter) Import(path string) (*types.Package, error) {
	return vi.ck.gc.Import(baseImportPath(path))
}

// check type-checks one package variant from source. Errors are collected, never fatal: the answer
// then says types=partial.
func (ck *checker) check(lp *listPackage) *checkedPkg {
	if ch, ok := ck.packages[lp.ImportPath]; ok {
		return ch
	}
	ch := &checkedPkg{lp: lp, model: map[token.Pos]funcSpan{}}
	ck.packages[lp.ImportPath] = ch
	var files []*ast.File
	for _, name := range lp.CompiledGoFiles {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(lp.Dir, p)
		}
		f, err := parser.ParseFile(ck.fset, p, nil, 0)
		if err != nil {
			ck.problem(err)
			continue
		}
		files = append(files, f)
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{
		Importer: &variantImporter{ck: ck},
		Error:    func(err error) { ck.problem(err) },
	}
	pkg, err := conf.Check(lp.ImportPath, ck.fset, files, info)
	if err != nil && pkg == nil {
		ck.problem(err)
		return ch
	}
	ch.pkg, ch.info, ch.files = pkg, info, files
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				name = baseReceiver(recvText(fn.Recv.List[0].Type)) + "." + name
			}
			ch.model[fn.Pos()] = funcSpan{start: fn.Pos(), end: fn.End(), name: name}
		}
	}
	return ch
}

// enclosing names the function a position sits in, which is what makes a hit readable.
func (ch *checkedPkg) enclosing(pos token.Pos) string {
	best := funcSpan{}
	for _, span := range ch.model {
		if pos >= span.start && pos <= span.end && (best.name == "" || span.end-span.start < best.end-best.start) {
			best = span
		}
	}
	return best.name
}

func (ck *checker) rel(file string) string {
	if rel, err := filepath.Rel(ck.ld.root, file); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(file)
}

// targetSpec is what a reference must denote: a name in one package, with the receiver when the symbol
// is a method or a field. The rules are structural on purpose: the same object can reach an answer as a
// source-checked declaration or through export data, and both must be recognised.
type targetSpec struct {
	basePkg string
	name    string
	class   string
	recv    string
}

func newTargetSpec(base string, defs []Decl) targetSpec {
	spec := targetSpec{basePkg: base}
	for _, d := range defs {
		spec.name = d.Name
		spec.class = d.Kind
		if recv := baseReceiver(d.Receiver); recv != "" {
			spec.recv = recv
		}
	}
	return spec
}

// match decides whether an object is the queried symbol.
func (t targetSpec) match(ck *checker, obj types.Object) bool {
	if obj == nil {
		return false
	}
	if obj.Name() != t.name || obj.Pkg() == nil {
		return false
	}
	if baseImportPath(obj.Pkg().Path()) != t.basePkg {
		return false
	}
	switch t.class {
	case "method":
		fn, ok := obj.(*types.Func)
		if !ok {
			return false
		}
		sig, ok := fn.Type().(*types.Signature)
		if !ok || sig.Recv() == nil {
			return false
		}
		return baseReceiver(typeBaseName(sig.Recv().Type())) == t.recv
	case "field":
		v, ok := obj.(*types.Var)
		if !ok || !v.IsField() {
			return false
		}
		// A qualified Type.field never matches a field whose struct is not a package-level type (owner "").
		if t.recv != "" {
			return fieldOwner(obj) == t.recv
		}
		return true
	case "func":
		fn, ok := obj.(*types.Func)
		if !ok {
			return false
		}
		sig, ok := fn.Type().(*types.Signature)
		return ok && sig.Recv() == nil
	// A var or const target is a package-level declaration; a function-local object with the same name is not it.
	case "const":
		c, ok := obj.(*types.Const)
		return ok && c.Parent() == c.Pkg().Scope()
	case "var":
		v, ok := obj.(*types.Var)
		return ok && !v.IsField() && v.Parent() == v.Pkg().Scope()
	}
	return true
}

// fieldOwner names the struct type that declares a field, so a field is never confused with a field of
// the same name on another type in the package.
func fieldOwner(obj types.Object) string {
	if obj.Pkg() == nil {
		return ""
	}
	for _, name := range obj.Pkg().Scope().Names() {
		tn, ok := obj.Pkg().Scope().Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok {
			continue
		}
		for i := 0; i < st.NumFields(); i++ {
			if st.Field(i) == obj {
				return tn.Name()
			}
		}
	}
	return ""
}

func typeBaseName(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}

func objectClass(obj types.Object) string {
	switch o := obj.(type) {
	case *types.Func:
		if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
			return "method"
		}
		return "func"
	case *types.Const:
		return "const"
	case *types.Var:
		if o.IsField() {
			return "field"
		}
		return "var"
	case *types.TypeName:
		return "type"
	}
	return "ref"
}

// refsIn collects every exact use of the target inside one checked package.
func (ck *checker) refsIn(ch *checkedPkg, spec targetSpec, callsOnly bool) []Site {
	if ch.pkg == nil || ch.info == nil {
		return nil
	}
	var out []Site
	seen := map[token.Pos]bool{}
	add := func(pos token.Pos, obj types.Object, call bool) {
		if !pos.IsValid() || seen[pos] {
			return
		}
		p := ck.fset.Position(pos)
		kind := objectClass(obj)
		if call {
			kind = "call"
		}
		seen[pos] = true
		out = append(out, Site{
			In:         ch.enclosing(pos),
			Kind:       kind,
			CallLine:   p.Line,
			Col:        p.Column,
			Confidence: "exact",
			Range:      Range{File: ck.rel(p.Filename), Start: p.Line, End: p.Line},
		})
	}
	callPos := map[token.Pos]bool{}
	for _, f := range ch.files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				callPos[fun.Pos()] = true
			case *ast.SelectorExpr:
				callPos[fun.Sel.Pos()] = true
			case *ast.IndexExpr:
				if id, ok := fun.X.(*ast.Ident); ok {
					callPos[id.Pos()] = true
				}
			case *ast.IndexListExpr:
				if id, ok := fun.X.(*ast.Ident); ok {
					callPos[id.Pos()] = true
				}
			}
			return true
		})
	}
	for id, obj := range ch.info.Uses {
		if !spec.match(ck, obj) {
			continue
		}
		add(id.Pos(), obj, callPos[id.Pos()])
	}
	for expr, selection := range ch.info.Selections {
		if selection == nil || !spec.match(ck, selection.Obj()) {
			continue
		}
		add(expr.Sel.Pos(), selection.Obj(), callPos[expr.Sel.Pos()])
	}
	if callsOnly {
		var kept []Site
		for _, s := range out {
			if s.Kind == "call" {
				kept = append(kept, s)
			}
		}
		out = kept
	}
	sortSites(out)
	return out
}

// ---------- engine B: refs and impl answers ----------

const (
	typesOK      = "ok"
	typesPartial = "partial"
	typesNoTypes = "unavailable"
)

type implHit struct {
	Decl
	Satisfies string `json:"satisfies"`
}

// typeAnswer is one symbol's type-checked answer; JSON and text are both rendered from it.
type typeAnswer struct {
	cmd        string
	symbol     string
	failure    map[string]any
	defs       []Decl
	candidates []candidate
	exact      []Site
	tests      []Site
	nameOnly   []Site
	impls      []implHit
	target     string
	verb       string
	typesState string
	problems   []string
	note       string
	ms         int64
}

type typedSession struct {
	se      *session
	answers []*typeAnswer
}

func (ld *loader) relDir(importPath string) string {
	for _, lp := range ld.base[baseImportPath(importPath)] {
		if rel, err := filepath.Rel(ld.root, lp.Dir); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return ""
}

// variantFor picks the package variant that contains a declaration: the plain build, the package with
// its internal test files, or the external (x_test) test package.
func (ld *loader) variantFor(base string, d Decl) *listPackage {
	if !inTestFile(d.File) {
		return ld.variant(base, "prod")
	}
	pkgName := ""
	for _, lp := range ld.base[base] {
		pkgName = lp.Name
		break
	}
	if d.Package != "" && pkgName != "" && d.Package != pkgName {
		return ld.variant(base, "xtest")
	}
	return ld.variant(base, "prod")
}

func (ix *index) notFound(spec string, cfg config, cmd string) map[string]any {
	name := spec
	if i := strings.LastIndex(spec, "."); i > 0 {
		name = spec[i+1:]
	}
	return map[string]any{
		"ok":          false,
		"cmd":         cmd,
		"symbol":      spec,
		"error":       "no declaration named " + spec,
		"scan":        map[string]any{"path": cfg.path, "files": len(ix.files)},
		"suggestions": ix.suggest(name),
	}
}

func (se *session) typedAnswer(spec string, callsOnly bool) *typeAnswer {
	cfg, ix := se.cfg, se.ix
	ans := &typeAnswer{cmd: cfg.cmd, symbol: spec, typesState: typesNoTypes}
	ids := ix.lookup(spec, cfg)
	if len(ids) == 0 {
		ans.failure = ix.notFound(spec, cfg, cfg.cmd)
		return ans
	}
	for _, id := range ids {
		ans.defs = append(ans.defs, ix.decls[id])
	}
	groups := groupDefs(ans.defs)
	if len(groups) > 1 {
		for _, g := range groups {
			ans.candidates = append(ans.candidates, candidate{Key: g.key, Defs: g.defs})
		}
		return ans
	}
	ans.defs = groups[0].defs
	ld, err := loadModule(cfg.root, cfg.path, !cfg.noTests)
	if err != nil {
		ans.note = fmt.Sprintf("engine B unavailable (%v); fell back to engine A, every hit is name_only", err)
		return se.typedFallback(ans, callsOnly)
	}
	absDir := filepath.Join(cfg.root, dirOf(ans.defs[0].File))
	base := ld.dirPath[absDir]
	if base == "" {
		ans.note = fmt.Sprintf("%s is not in a package go list compiles; fell back to engine A, every hit is name_only", dirOf(ans.defs[0].File))
		return se.typedFallback(ans, callsOnly)
	}
	ck := newChecker(ld)
	defer func() { ans.ms = time.Since(se.started).Milliseconds() }()
	want := newTargetSpec(base, ans.defs)
	if cfg.cmd == "impl" {
		return se.implAnswer(ans, ck, ld, want)
	}
	var variants []*listPackage
	if v := ld.variant(base, "prod"); v != nil {
		variants = append(variants, v)
	}
	if !cfg.noTests {
		for _, kind := range []string{"aug", "xtest"} {
			if v := ld.variant(base, kind); v != nil {
				variants = append(variants, v)
			}
		}
	}
	var scope []*listPackage
	seenVariant := map[string]bool{}
	for _, v := range variants {
		if v != nil && !seenVariant[v.ImportPath] {
			seenVariant[v.ImportPath] = true
			scope = append(scope, v)
		}
	}
	for _, imp := range ld.importersOf(base) {
		if seenVariant[imp.ImportPath] {
			continue
		}
		seenVariant[imp.ImportPath] = true
		scope = append(scope, imp)
	}
	var hits []Site
	for _, lp := range scope {
		hits = append(hits, ck.refsIn(ck.check(lp), want, callsOnly)...)
	}
	ans.exact, ans.tests = splitTestSites(dedupeTypedSites(hits))
	ans.nameOnly = ck.buildTagged(ld, scope, want)
	ans.typesState = typesOK
	if ck.partial {
		ans.typesState = typesPartial
		ans.problems = ck.problems
	}
	if len(ans.exact) == 0 && len(ans.tests) == 0 && len(ans.nameOnly) == 0 && !ck.partial {
		ans.note = "no type-checked references in the defining package or its importers (scope: " + itoa(len(scope)) + " packages)"
	}
	if cfg.cmd == "refs" && ans.defs[0].Kind == "method" {
		if names := ck.interfacesDispatching(ld, ix.packagesDeclaring(ld, interfaceDeclaring(want.name)), ck.methodObject(base, want)); len(names) > 0 {
			ans.note = "calls through interface " + strings.Join(names, ", ") + " not attributed; see impl " + names[0]
		}
	}
	return ans
}

// interfaceDeclaring selects an interface declaration that has a method of the given name.
func interfaceDeclaring(name string) func(Decl) bool {
	return func(d Decl) bool {
		return d.Kind == "interface" && slices.Contains(d.Methods, name)
	}
}

// methodObject returns the concrete method the target names, from the export data of its package.
func (ck *checker) methodObject(base string, want targetSpec) *types.Func {
	pkg, err := ck.gc.Import(base)
	if err != nil || pkg == nil {
		return nil
	}
	tn, ok := pkg.Scope().Lookup(want.recv).(*types.TypeName)
	if !ok {
		return nil
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return nil
	}
	for i := 0; i < named.NumMethods(); i++ {
		if m := named.Method(i); m.Name() == want.name {
			return m
		}
	}
	return nil
}

// interfacesDispatching names up to three interfaces, in the given packages, whose single method has the
// target's name and a signature the target's pointer type satisfies. Such an interface can reach the
// method without naming it. The check is types.Implements on a one-method interface, so the signatures
// compare by the same identities impl uses, aliases included.
func (ck *checker) interfacesDispatching(ld *loader, bases []string, method *types.Func) []string {
	if method == nil {
		return nil
	}
	recv := method.Type().(*types.Signature).Recv().Type()
	var names []string
	for _, base := range bases {
		lp := ld.variant(base, "prod")
		if lp == nil {
			continue
		}
		ch := ck.check(lp)
		if ch == nil || ch.pkg == nil {
			continue
		}
		for _, name := range ch.pkg.Scope().Names() {
			tn, ok := ch.pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() || len(names) >= 3 {
				continue
			}
			// The export-data twin, the identity the method was read with, as impl does.
			twin := ck.exportedType(base, name)
			if twin == nil {
				continue
			}
			iface, ok := twin.Underlying().(*types.Interface)
			if !ok || iface.NumMethods() == 0 || slices.Contains(names, name) {
				continue
			}
			for i := 0; i < iface.NumMethods(); i++ {
				if iface.Method(i).Name() != method.Name() {
					continue
				}
				single := types.NewInterfaceType([]*types.Func{iface.Method(i)}, nil)
				single.Complete()
				if types.Implements(recv, single) {
					names = append(names, name)
					break
				}
			}
		}
	}
	return names
}

// typedFallback answers with engine A when engine B cannot run, never silently: every hit is marked
// name_only and the note says why.
func (se *session) typedFallback(ans *typeAnswer, callsOnly bool) *typeAnswer {
	cfg, ix := se.cfg, se.ix
	tg := target{name: ans.defs[0].Name, defLine: map[string]bool{}, declSet: keysOf(ix.byName), typeSet: keysOf(ix.typeIdx), refs: true}
	for _, d := range ans.defs {
		tg.defs = append(tg.defs, d.Range)
		if d.Kind == "const" || d.Kind == "var" || d.Kind == "field" {
			tg.defLine[fmt.Sprintf("%s#%d", d.File, d.Start)] = true
		}
	}
	sc := ix.scanRefs(cfg, tg)
	var hits []Site
	for _, site := range ix.resolveHits(sc.hits, map[string]bool{dirOf(ans.defs[0].File): true}, false, false) {
		if callsOnly && site.Kind != "call" && site.Kind != "selector_call" {
			continue
		}
		site.Confidence = "name_only"
		hits = append(hits, site)
	}
	ans.exact, ans.tests = splitTestSites(dedupeTypedSites(hits))
	ans.ms = time.Since(se.started).Milliseconds()
	return ans
}

func splitTestSites(sites []Site) (prod, tests []Site) {
	for _, s := range sites {
		if inTestFile(s.File) {
			tests = append(tests, s)
			continue
		}
		prod = append(prod, s)
	}
	return
}

func dedupeTypedSites(in []Site) []Site {
	seen := map[string]bool{}
	var out []Site
	for _, s := range in {
		key := fmt.Sprintf("%s#%d#%d", s.File, s.CallLine, s.Col)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	sortSites(out)
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// buildTagged scans the files go list excluded by build tags with engine A; those hits keep the name
// they matched but cannot be typed, so they are reported separately as name_only.
func (ck *checker) buildTagged(ld *loader, scope []*listPackage, want targetSpec) []Site {
	var out []Site
	seen := map[string]bool{}
	for _, lp := range scope {
		for _, name := range lp.IgnoredGoFiles {
			if !strings.HasSuffix(name, ".go") {
				continue
			}
			abs := name
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(lp.Dir, abs)
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, abs, nil, 0)
			if err != nil {
				continue
			}
			tg := target{name: want.name, refs: true}
			hits, _, _, _ := scanFile(fset, file, ck.rel(abs), tg)
			for _, h := range hits {
				site := Site{Kind: "name_only", Confidence: "name_only", CallLine: h.line, Range: Range{File: ck.rel(abs), Start: h.line, End: h.line}, In: h.in}
				key := fmt.Sprintf("%s#%d#%s", site.File, site.CallLine, site.In)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, site)
			}
		}
	}
	sortSites(out)
	return out
}

// implAnswer answers impl in both directions: an interface lists its implementers, any other type
// lists the interfaces it satisfies. types.Implements is asked for T and for *T.
// implAnswer answers impl in both directions: an interface lists its implementers, any other type lists
// the interfaces it satisfies. types.Implements decides, asked for T and for *T, and the packages that
// are type-checked are only the ones engine A shows could possibly answer (a method or interface with a
// matching name), so unexported types are seen without type-checking the whole module.
func (se *session) implAnswer(ans *typeAnswer, ck *checker, ld *loader, want targetSpec) *typeAnswer {
	cfg, ix := se.cfg, se.ix
	def := ans.defs[0]
	if !isTypeKind(def.Kind) {
		ans.failure = map[string]any{
			"ok": false, "cmd": "impl", "symbol": ans.symbol,
			"error": fmt.Sprintf("impl needs a type or interface, %s is a %s", ans.symbol, def.Kind),
		}
		return ans
	}
	declFor := func(importPath, name string) (Decl, bool) {
		dir := ld.relDir(importPath)
		for _, id := range ix.byName[name] {
			d := ix.decls[id]
			if isTypeKind(d.Kind) && dirOf(d.File) == dir {
				return d, true
			}
		}
		return Decl{}, false
	}
	ans.typesState = typesOK
	if ck.partial {
		ans.typesState = typesPartial
		ans.problems = ck.problems
	}
	// The target type is taken from export data so its signatures carry the same package identities
	// every candidate sees; an unexported type is only visible in its own source-checked package.
	target := ck.exportedType(want.basePkg, def.Name)
	sourceChecked := false
	if target == nil {
		if ch := ck.check(ld.variantFor(want.basePkg, def)); ch != nil && ch.pkg != nil {
			if obj := ch.pkg.Scope().Lookup(def.Name); obj != nil {
				target = obj.Type()
				sourceChecked = true
			}
		}
	}
	if sourceChecked {
		ans.note = def.Name + " is not exported, so it was resolved from source: implementers outside its package may be missed"
	}
	if target == nil {
		ans.impls = engineAImpls(ix.implementers(def))
		ans.note = "no type information for " + def.Name + "; engine A implementers (by method name) marked name_only"
		return ans
	}
	named, ok := target.(*types.Named)
	if !ok {
		ans.failure = map[string]any{"ok": false, "cmd": "impl", "symbol": ans.symbol, "error": def.Name + " is not a named type"}
		return ans
	}
	iface, isInterface := named.Underlying().(*types.Interface)
	if isInterface && iface.NumMethods() == 0 {
		ans.verb = "implementer"
		ans.note = def.Name + " is an empty interface: every type implements it, so impl does not list them"
		return ans
	}
	// checkAll type-checks every variant of the candidate packages and hands each package to fn.
	skipped := 0
	checkAll := func(bases []string, fn func(ch *checkedPkg)) {
		for _, base := range bases {
			kinds := []string{"prod"}
			if !cfg.noTests {
				kinds = append(kinds, "aug", "xtest")
			}
			for _, kind := range kinds {
				lp := ld.variant(base, kind)
				if lp == nil {
					skipped++
					continue
				}
				if ch := ck.check(lp); ch != nil && ch.pkg != nil {
					fn(ch)
				} else {
					skipped++
				}
			}
		}
		if ck.partial {
			ans.typesState = typesPartial
			ans.problems = ck.problems
		}
	}
	names := func(methods []string) map[string]bool {
		out := map[string]bool{}
		for _, m := range methods {
			out[m] = true
		}
		return out
	}
	if isInterface {
		ans.verb = "implementer"
		wanted := make([]string, 0, iface.NumMethods())
		for i := 0; i < iface.NumMethods(); i++ {
			wanted = append(wanted, iface.Method(i).Name())
		}
		byName := names(wanted)
		// Only a package that declares a method of that name can declare an implementer.
		candidates := ix.packagesDeclaring(ld, func(d Decl) bool { return d.Kind == "method" && byName[d.Name] })
		checkAll(candidates, func(ch *checkedPkg) {
			for _, name := range ch.pkg.Scope().Names() {
				tn, ok := ch.pkg.Scope().Lookup(name).(*types.TypeName)
				if !ok || tn.IsAlias() {
					continue
				}
				candNamed, ok := tn.Type().(*types.Named)
				if !ok {
					continue
				}
				if candNamed.Obj().Name() == def.Name && candNamed.Obj().Pkg() != nil && baseImportPath(candNamed.Obj().Pkg().Path()) == want.basePkg {
					continue
				}
				// Compare with the export-data twin, the identity the interface was read with: a source-checked
				// named type never satisfies it when its signatures mention another named type of the package.
				implType := types.Type(candNamed)
				if twin := ck.exportedType(ch.pkg.Path(), name); twin != nil {
					implType = twin
				}
				sat := satisfies(implType, iface)
				if sat == "" {
					continue
				}
				if d, ok := declFor(ch.pkg.Path(), name); ok {
					ans.impls = append(ans.impls, implHit{Decl: d, Satisfies: sat})
				}
			}
		})
	} else {
		ans.verb = "interface"
		// Only an interface whose methods all appear on the type can be satisfied by it.
		have := map[string]bool{}
		collect := func(t types.Type) {
			ms := types.NewMethodSet(t)
			for i := 0; i < ms.Len(); i++ {
				have[ms.At(i).Obj().Name()] = true
			}
		}
		collect(named)
		collect(types.NewPointer(named))
		candidates := ix.packagesDeclaring(ld, func(d Decl) bool {
			if d.Kind != "interface" || len(d.Methods) == 0 {
				return false
			}
			for _, m := range d.Methods {
				if !have[m] {
					return false
				}
			}
			return true
		})
		checkAll(candidates, func(ch *checkedPkg) {
			for _, name := range ch.pkg.Scope().Names() {
				tn, ok := ch.pkg.Scope().Lookup(name).(*types.TypeName)
				if !ok || tn.IsAlias() {
					continue
				}
				candIface, ok := tn.Type().Underlying().(*types.Interface)
				if !ok || candIface.NumMethods() == 0 {
					continue
				}
				// Prefer the exported twin: its signatures carry the identities candidates agree on.
				if twin := ck.exportedType(ch.pkg.Path(), name); twin != nil {
					if exported, ok := twin.Underlying().(*types.Interface); ok && exported.NumMethods() > 0 {
						candIface = exported
					}
				}
				sat := satisfies(named, candIface)
				if sat == "" {
					continue
				}
				if d, ok := declFor(ch.pkg.Path(), name); ok {
					ans.impls = append(ans.impls, implHit{Decl: d, Satisfies: sat})
				}
			}
		})
	}
	ans.impls = dedupeImpls(ans.impls)
	if len(ans.impls) == 0 && ans.typesState == typesOK {
		if isInterface && skipped > 0 {
			ans.note = plural(skipped, "candidate package") + " could not be type-checked, so no implementer is claimed"
		} else if isInterface {
			ans.note = "no type among the packages that declare a method of that name implements it"
		} else {
			ans.note = "no interface among the packages that declare those method names is satisfied by " + def.Name
		}
	}
	return ans
}

// exportedType looks a named type up in the export data of its package, the identity every candidate
// package agrees on, and returns nil when the type is not exported.
func (ck *checker) exportedType(base, name string) types.Type {
	pkg, err := ck.gc.Import(base)
	if err != nil || pkg == nil {
		return nil
	}
	if obj := pkg.Scope().Lookup(name); obj != nil {
		return obj.Type()
	}
	return nil
}

// packagesDeclaring lists the module package directories whose engine A declarations satisfy pred.
func (ix *index) packagesDeclaring(ld *loader, pred func(Decl) bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range ix.decls {
		if !pred(d) {
			continue
		}
		dir := filepath.Join(ix.root, dirOf(d.File))
		base, ok := ld.dirPath[dir]
		if !ok || seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, base)
	}
	sort.Strings(out)
	return out
}

// satisfies says how a type meets an interface: as a value, as a pointer, or both.
// engineAImpls marks structural (name-only) implementers, so a fallback never looks exact.
func engineAImpls(decls []Decl) []implHit {
	out := make([]implHit, 0, len(decls))
	for _, d := range decls {
		out = append(out, implHit{Decl: d, Satisfies: "name_only"})
	}
	return out
}

func satisfies(t types.Type, iface *types.Interface) string {
	value := types.Implements(t, iface)
	pointer := types.Implements(types.NewPointer(t), iface)
	switch {
	case value && pointer:
		return "both"
	case pointer:
		return "pointer"
	case value:
		return "value"
	}
	return ""
}

func dedupeImpls(in []implHit) []implHit {
	seen := map[string]bool{}
	var out []implHit
	for _, h := range in {
		key := declKey(h.Decl)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Start < out[j].Start
	})
	return out
}

// ---------- engine B: text and JSON output ----------

func (ans *typeAnswer) subject() string {
	d := ans.defs[0]
	recv := baseReceiver(d.Receiver)
	switch {
	case recv != "":
		return d.Name + " (*" + recv + ")"
	case d.Kind != "func" && d.Kind != "method" && d.Package != "":
		return d.Name + " (" + d.Package + ")"
	}
	return d.Name
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func refLine(s Site) string {
	at := fmt.Sprintf("%s:%d", s.File, s.CallLine)
	if s.Col > 0 {
		at = fmt.Sprintf("%s:%d:%d", s.File, s.CallLine, s.Col)
	}
	return fmt.Sprintf("%s\t%s\tin=%s\n", at, s.Kind, strings.TrimSpace(s.In))
}

func (ans *typeAnswer) summary() string {
	total := len(ans.exact) + len(ans.tests)
	switch ans.cmd {
	case "impl":
		return plural(len(ans.impls), ans.verb)
	case "callers":
		return plural(total, "caller")
	}
	return plural(total, "ref")
}

func (te *typedSession) renderText(ans *typeAnswer) string {
	cfg := te.se.cfg
	if ans.failure != nil {
		return renderError(ans.failure)
	}
	if len(ans.candidates) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s: ambiguous, %d candidates; pass one key (pkg.Name or Type.Method)\n", ans.cmd, ans.symbol, len(ans.candidates))
		for _, c := range ans.candidates {
			for _, d := range c.Defs {
				b.WriteString(declLine(d, c.Key))
			}
		}
		return b.String()
	}
	var b strings.Builder
	shown := len(ans.exact) + len(ans.tests)
	if ans.cmd == "impl" {
		shown = len(ans.impls)
	}
	scope := ""
	if ans.cmd != "impl" && len(ans.tests) > 0 {
		scope = fmt.Sprintf(", tests=%d", len(ans.tests))
	}
	fmt.Fprintf(&b, "# %s %s: %s, shown %d%s, types=%s, %dms\n", ans.cmd, ans.subject(), ans.summary(), shown, scope, ans.typesState, ans.ms)
	if ans.cmd == "impl" {
		for i, h := range ans.impls {
			if i >= cfg.max {
				fmt.Fprintf(&b, "... %d more %s; use --max N\n", len(ans.impls)-cfg.max, ans.verb+"s")
				break
			}
			fmt.Fprintf(&b, "%s:%d-%d\t%s\t%s\tsatisfies=%s\n", h.File, h.Start, h.End, h.Kind, displayKey(h.Decl, false), h.Satisfies)
		}
	} else {
		for i, s := range ans.exact {
			if i >= cfg.max {
				fmt.Fprintf(&b, "... %d more; use --max N\n", len(ans.exact)-cfg.max)
				break
			}
			b.WriteString(refLine(s))
		}
		if len(ans.tests) > 0 {
			fmt.Fprintf(&b, "# tests: %d (grouped; --no-tests skips them)\n", len(ans.tests))
			for i, s := range ans.tests {
				if i >= cfg.max {
					fmt.Fprintf(&b, "... %d more test refs; use --max N\n", len(ans.tests)-cfg.max)
					break
				}
				b.WriteString(refLine(s))
			}
		}
		if len(ans.nameOnly) > 0 {
			fmt.Fprintf(&b, "# name_only: %s in files excluded by build tags (engine A, no type info)\n", plural(len(ans.nameOnly), "hit"))
			for i, s := range ans.nameOnly {
				if i >= cfg.max {
					fmt.Fprintf(&b, "... %d more name_only hits; use --max N\n", len(ans.nameOnly)-cfg.max)
					break
				}
				b.WriteString(refLine(s))
			}
		}
	}
	if ans.note != "" {
		fmt.Fprintf(&b, "# note: %s\n", ans.note)
	}
	for _, p := range ans.problems {
		fmt.Fprintf(&b, "# types=%s: %s\n", ans.typesState, p)
	}
	return b.String()
}

func (te *typedSession) toJSON(ans *typeAnswer) map[string]any {
	if ans.failure != nil {
		return ans.failure
	}
	scan := map[string]any{"path": te.se.cfg.path, "files": len(te.se.ix.files), "engine": "go/types", "ms": ans.ms}
	if len(ans.candidates) > 0 {
		return map[string]any{
			"ok":          true,
			"cmd":         ans.cmd,
			"symbol":      ans.symbol,
			"ambiguous":   true,
			"definitions": ans.defs,
			"candidates":  ans.candidates,
			"ranges":      rangesOf(ans.defs),
			"note":        "several definitions share this name; pass pkg.Name or Type.Method to pick one",
			"scan":        scan,
		}
	}
	truncated := map[string]bool{}
	out := map[string]any{
		"ok":          true,
		"cmd":         ans.cmd,
		"symbol":      ans.symbol,
		"engine":      "go/types",
		"types":       ans.typesState,
		"definitions": ans.defs,
		"counts": map[string]int{
			"refs":      len(ans.exact) + len(ans.tests),
			"callers":   len(ans.exact),
			"tests":     len(ans.tests),
			"name_only": len(ans.nameOnly),
			"impls":     len(ans.impls),
		},
		"scan": scan,
	}
	if ans.cmd == "impl" {
		out["impls"] = ans.impls
		out["direction"] = ans.verb
	} else {
		out["refs"] = bound(te.se.cfg.max, ans.exact, truncated, "refs")
		out["tests"] = bound(te.se.cfg.max, ans.tests, truncated, "tests")
		out["name_only"] = bound(te.se.cfg.max, ans.nameOnly, truncated, "name_only")
		out["ranges"] = chainRanges(ans.defs, ans.exact, ans.tests)
	}
	if len(truncated) > 0 {
		out["truncated"] = truncated
	}
	if ans.note != "" {
		out["note"] = ans.note
	}
	if len(ans.problems) > 0 {
		out["problems"] = ans.problems
	}
	return out
}

// ---------- engine B: entry points ----------

func typedSessionOf(cfg config, started time.Time) (*typedSession, error) {
	se, specs, err := openSession(cfg, started)
	if err != nil {
		return nil, err
	}
	ts := &typedSession{se: se}
	callsOnly := se.cfg.cmdcalls()
	for _, spec := range specs {
		ts.answers = append(ts.answers, se.typedAnswer(spec, callsOnly))
	}
	return ts, nil
}

func (cfg config) cmdcalls() bool { return cfg.calls || cfg.cmd == "callers" }

func (ts *typedSession) jsonResult() (any, error) {
	results := make([]map[string]any, 0, len(ts.answers))
	unresolved := 0
	for _, ans := range ts.answers {
		if ans.failure != nil || len(ans.candidates) > 0 {
			unresolved++
		}
		results = append(results, ts.toJSON(ans))
	}
	if len(results) == 1 {
		out := results[0]
		if ts.answers[0].failure != nil || len(ts.answers[0].candidates) > 0 {
			return nil, &resultError{value: out}
		}
		return out, nil
	}
	out := map[string]any{"ok": unresolved < len(results), "cmd": ts.se.cfg.cmd, "results": results}
	if unresolved == len(results) {
		return nil, &resultError{value: out}
	}
	return out, nil
}

func typedResult(cfg config, started time.Time) (any, error) {
	ts, err := typedSessionOf(cfg, started)
	if err != nil {
		return nil, err
	}
	return ts.jsonResult()
}

func typedText(cfg config, started time.Time) (string, error) {
	ts, err := typedSessionOf(cfg, started)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	unresolved := 0
	for _, ans := range ts.answers {
		if ans.failure != nil || len(ans.candidates) > 0 {
			unresolved++
		}
		b.WriteString(ts.renderText(ans))
	}
	text := capText(b.String(), ts.se.cfg, "narrow with pkg.Name, --max N or --no-tests")
	if unresolved == len(ts.answers) {
		return "", &resultError{value: map[string]any{"text": text}}
	}
	return text, nil
}
