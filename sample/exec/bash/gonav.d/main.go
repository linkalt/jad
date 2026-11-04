// gonav: deterministic Go navigation from the compiler's own parser (go/ast). Stdlib only.
// Prints exactly one JSON object; every range is {"file","start_line","end_line"} relative to the
// workspace root, so any answer chains into "gonav source" (or the bash helper read.sh).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
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
		if !cfg.json {
			fmt.Printf("error: %v\n", err)
			os.Exit(1)
		}
		fail(cfg.cmd, err)
	}
	if !cfg.json {
		text, err := executeText(cfg, started)
		if err != nil {
			var payload *resultError
			if errors.As(err, &payload) {
				fmt.Print(capText(renderError(payload.value), cfg, ""))
				os.Exit(1)
			}
			fmt.Printf("error: %v\n", err)
			os.Exit(1)
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
		fail(cfg.cmd, err)
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
	case "source":
		return sourceResult(cfg, started)
	}
	return nil, fmt.Errorf("unknown command %q; use navigate, symbol, impact, show or source", cfg.cmd)
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
		case "--no-tests":
			cfg.noTests = true
		case "--json":
			cfg.json = true
		case "--help", "-h":
			return cfg, fmt.Errorf("usage: gonav navigate|symbol|impact|show SYM [SYM...] (SYM: Name, Type.Field, Type.Method, pkg.Name; a|b|c ok) [--path DIR] [--receiver R] [--max N] [--max-lines N] [--max-bytes N] [--no-tests] [--json] | gonav source [FILE START END] (JSON ranges on stdin)")
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

// lookup resolves Name, Type.Member, pkg.Name or pkg.Type.Member to declaration indices.
func (ix *index) lookup(spec string, cfg config) []int {
	name, qual := spec, ""
	if i := strings.LastIndex(spec, "."); i > 0 {
		qual, name = spec[:i], spec[i+1:]
	}
	var ids []int
	for _, id := range ix.byName[name] {
		d := ix.decls[id]
		if cfg.noTests && inTestFile(d.File) {
			continue
		}
		if cfg.receiver != "" && d.Receiver != "" && baseReceiver(d.Receiver) != cfg.receiver {
			continue
		}
		if qual != "" {
			recv := baseReceiver(d.Receiver)
			if qual != d.Package && (recv == "" || (qual != recv && qual != d.Package+"."+recv)) {
				continue
			}
		}
		ids = append(ids, id)
	}
	return ids
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

func buildSession(cfg config, started time.Time) (*session, error) {
	cmd := cfg.cmd
	if cmd == "" {
		cmd = "navigate"
	}
	cfg.cmd = cmd
	specs := symbolList(cfg)
	if len(specs) == 0 {
		return nil, fmt.Errorf("no symbol given; usage: gonav %s SYMBOL [SYMBOL...]", cmd)
	}
	ix, err := openIndex(&cfg)
	if err != nil {
		return nil, err
	}
	se := &session{cfg: cfg, ix: ix, started: started}
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
			"ok":          true,
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
		if ans.failure != nil {
			return nil, &resultError{value: out}
		}
		return out, nil
	}
	results := make([]map[string]any, 0, len(se.answers))
	failed := 0
	for _, ans := range se.answers {
		if ans.failure != nil {
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
	return out, nil
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
	case "navigate", "symbol", "impact", "show", "":
		se, err := buildSession(cfg, started)
		if err != nil {
			return "", err
		}
		if se.cfg.cmd == "show" {
			ranges, notes := se.showRanges()
			blocks, _, omitted := readBlocks(se.cfg, ranges)
			return capText(strings.Join(notes, "")+renderBlocks(blocks, omitted), se.cfg, ""), nil
		}
		var b strings.Builder
		failed := 0
		for _, ans := range se.answers {
			if ans.failure != nil {
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
	return "", fmt.Errorf("unknown command %q; use navigate, symbol, impact, show or source", cfg.cmd)
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
