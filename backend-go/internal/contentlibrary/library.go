package contentlibrary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
)

// Kind identifies one root-library directory family.
type Kind string

const (
	Prompt   Kind = "prompt"
	Template Kind = "template"
	Skill    Kind = "skill"
	// Command is a custom slash command: a Markdown file whose text a person sends by
	// typing /<command> (see commands.go).
	Command Kind = "command"
)

// Summary is the metadata shown in the frontend's library catalog.
type Summary struct {
	ID          string `json:"id"`
	Kind        Kind   `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	CommandFields
}

// CommandFields are set on a command only.
type CommandFields struct {
	// Command is the word typed after the slash: the file's path below its command
	// folder without .md, each folder and the file name made a slug, joined by ":".
	Command string `json:"command,omitempty"`
	// ArgumentHint is the front matter's argument-hint, shown where the arguments go.
	ArgumentHint string `json:"argument_hint,omitempty"`
	// TakesArguments is true when the text has $ARGUMENTS or $1 to $9.
	TakesArguments bool `json:"takes_arguments,omitempty"`
	CommandOptions
	// Invalid says what is wrong with the front matter's run options; such a command is
	// listed, so the person sees it, but expanding it is refused with this text.
	Invalid string `json:"invalid,omitempty"`
}

// CommandOptions are how a command's front matter asks its turn to run. A client that
// sends the command applies them unless the person chose otherwise for that run.
type CommandOptions struct {
	// Model is a model reference (<category>/<name>, optionally @<effort>).
	Model string `json:"model,omitempty"`
	// Profile is the profile to run in, <category>/<name> or a path profile's name.
	Profile string `json:"profile,omitempty"`
	// Plan runs the turn in plan mode.
	Plan bool `json:"plan,omitempty"`
	// AllowedTools narrows the turn's tools to these tools.allow entries.
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

// Asset includes the full Markdown body returned when a user selects an item.
type Asset struct {
	ID          string `json:"id"`
	Kind        Kind   `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Content     string `json:"content"`
	CommandFields
}

// Catalog has one ordered list for each supported library kind.
type Catalog struct {
	Prompts   []Summary `json:"prompts"`
	Templates []Summary `json:"templates"`
	Skills    []Summary `json:"skills"`
	// Commands are the custom slash commands, which the "/" menu lists; the library panel
	// does not.
	Commands []Summary `json:"commands"`
	// CommandsError says why the commands could not be listed (a command folder that is a
	// file, a read error); Commands is then empty and the other kinds are still listed.
	CommandsError string `json:"commands_error,omitempty"`
	// Ambiguous (workspace layout only) names the skills and commands found in both the
	// global and the workspace library, which are left out of the lists.
	Ambiguous []string `json:"ambiguous,omitempty"`
}

// Library reads the configured root-library directories. Configuration and
// assets are loaded on each request so edits can be picked up by Refresh.
type Library struct {
	options Options
}

// New constructs a library service without touching the filesystem.
func New(options Options) *Library {
	options.Environment = cloneEnvironment(options.Environment)
	return &Library{options: options}
}

// List reads summaries from the configured prompt, template, and skill trees.
// In the workspace layout, workspace names whose library is listed ("" for the start
// workspace); a v1 library has none and refuses any other.
func (l *Library) List(ctx context.Context, workspace string) (Catalog, error) {
	if l == nil {
		return Catalog{}, errors.New("root library is unavailable")
	}
	if ctx == nil {
		return Catalog{}, errors.New("root library request context is required")
	}
	if l.options.Workspaces != nil {
		libraries, err := l.options.workspaceLibraries(workspace)
		if err != nil {
			return Catalog{}, err
		}
		dirs, err := scopedDirectoriesOf(libraries)
		if err != nil {
			return Catalog{}, err
		}
		return listScoped(ctx, dirs)
	}
	if workspace != "" {
		return Catalog{}, ErrUnknownWorkspace
	}
	directories, err := loadDirectories(l.options)
	if err != nil {
		return Catalog{}, err
	}
	catalog := Catalog{
		Prompts:   []Summary{},
		Templates: []Summary{},
		Skills:    []Summary{},
	}
	// The command folders were added later, with a default other tools may also use, so a
	// broken one costs only the commands, not the prompts, templates and skills beside them.
	commands, err := listCommands(ctx, directories.commands, directories.maxAssetBytes)
	switch {
	case err != nil && ctx.Err() != nil:
		return Catalog{}, ctx.Err()
	case err != nil:
		catalog.Commands, catalog.CommandsError = []Summary{}, err.Error()
	default:
		catalog.Commands = commands
	}
	for _, entry := range []struct {
		kind Kind
		dirs []string
		dst  *[]Summary
	}{
		{kind: Prompt, dirs: directories.prompts, dst: &catalog.Prompts},
		{kind: Template, dirs: directories.templates, dst: &catalog.Templates},
		{kind: Skill, dirs: directories.skills, dst: &catalog.Skills},
	} {
		items, err := listKind(ctx, entry.kind, entry.dirs, directories.maxAssetBytes)
		if err != nil {
			return Catalog{}, err
		}
		*entry.dst = items
	}
	return catalog, nil
}

// Read returns one current asset by its opaque, path-derived identifier, of workspace's
// library in the workspace layout (see List).
func (l *Library) Read(ctx context.Context, kind Kind, id, workspace string) (Asset, bool, error) {
	if l == nil || !validKind(kind) || !assetIDPattern.MatchString(id) {
		return Asset{}, false, nil
	}
	if ctx == nil {
		return Asset{}, false, errors.New("root library request context is required")
	}
	if l.options.Workspaces != nil {
		libraries, err := l.options.workspaceLibraries(workspace)
		if err != nil {
			return Asset{}, false, err
		}
		dirs, err := scopedDirectoriesOf(libraries)
		if err != nil {
			return Asset{}, false, err
		}
		return readScoped(ctx, dirs, kind, id)
	}
	if workspace != "" {
		return Asset{}, false, ErrUnknownWorkspace
	}
	directories, err := loadDirectories(l.options)
	if err != nil {
		return Asset{}, false, err
	}
	return readByID(ctx, kind, directoryList(directories, kind), id, directories.maxAssetBytes)
}

// readByID finds the asset of kind whose path-derived ID is id in directories.
func readByID(ctx context.Context, kind Kind, directories []string, id string, maxBytes int64) (Asset, bool, error) {
	files, err := filesForKind(ctx, kind, directories)
	if err != nil {
		return Asset{}, false, err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return Asset{}, false, err
		}
		if assetID(kind, file.path) != id {
			continue
		}
		asset, err := readAsset(kind, file.root, file.path, maxBytes)
		if err != nil {
			return Asset{}, false, err
		}
		if asset == nil {
			return Asset{}, false, nil
		}
		return *asset, true, nil
	}
	return Asset{}, false, nil
}

var (
	assetIDPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	skillNamePattern  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	frontmatterField  = regexp.MustCompile(`^(name|title|description|argument-hint|model|profile|plan|allowed-tools):\s*(.*)$`)
	inlineComment     = regexp.MustCompile(`\s+#.*$`)
	firstHeading      = regexp.MustCompile(`^#\s+(.+)$`)
	blankParagraph    = regexp.MustCompile(`\r?\n\s*\r?\n`)
	whitespaceRun     = regexp.MustCompile(`\s+`)
	separatorSequence = regexp.MustCompile(`[-_]+`)
)

type libraryFile struct {
	root string
	path string
}

type parsedMarkdown struct {
	name         string
	title        string
	description  string
	body         string
	argumentHint string
	// The run options of a command (see commandOptions); read for every kind, used by
	// commands only.
	model, profile, plan string
	allowedTools         []string
	hasAllowedTools      bool
	hasName              bool
	hasTitle             bool
	hasDesc              bool
}

func listKind(ctx context.Context, kind Kind, directories []string, maxBytes int64) ([]Summary, error) {
	files, err := filesForKind(ctx, kind, directories)
	if err != nil {
		return nil, err
	}
	items := make([]Summary, 0, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		asset, err := readAsset(kind, file.root, file.path, maxBytes)
		if err != nil {
			return nil, err
		}
		if asset != nil {
			items = append(items, Summary{
				ID:          asset.ID,
				Kind:        asset.Kind,
				Name:        asset.Name,
				Description: asset.Description,
				Source:      asset.Source,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if compared := localeCompare(items[i].Name, items[j].Name); compared != 0 {
			return compared < 0
		}
		return localeCompare(items[i].Source, items[j].Source) < 0
	})
	return items, nil
}

func filesForKind(ctx context.Context, kind Kind, directories []string) ([]libraryFile, error) {
	seen := make(map[string]struct{})
	files := make([]libraryFile, 0)
	for _, root := range directories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Stat(root)
		if err != nil {
			if isSkippableFilesystemError(err) {
				continue
			}
			return nil, err
		}
		if !info.IsDir() {
			return nil, configError("a library directory for %s is not a directory", kind)
		}
		var paths []string
		if kind == Skill {
			paths, err = skillFiles(ctx, root)
		} else {
			paths, err = markdownFiles(ctx, root)
		}
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			if _, exists := seen[path]; exists {
				continue
			}
			seen[path] = struct{}{}
			files = append(files, libraryFile{root: root, path: path})
		}
	}
	sort.Slice(files, func(i, j int) bool { return localeCompare(files[i].path, files[j].path) < 0 })
	return files, nil
}

func markdownFiles(ctx context.Context, root string) ([]string, error) {
	pending := []string{root}
	files := make([]string, 0)
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := os.ReadDir(directory)
		if err != nil {
			if isSkippableFilesystemError(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			path := filepath.Join(directory, entry.Name())
			if entry.IsDir() {
				pending = append(pending, path)
				continue
			}
			if !entry.Type().IsRegular() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			files = append(files, path)
		}
	}
	sort.Slice(files, func(i, j int) bool { return localeCompare(files[i], files[j]) < 0 })
	return files, nil
}

func skillFiles(ctx context.Context, root string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if isSkippableFilesystemError(err) {
			return []string{}, nil
		}
		return nil, err
	}
	files := make([]string, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "SKILL.md")
		info, err := os.Lstat(path)
		if err != nil {
			if isSkippableFilesystemError(err) {
				continue
			}
			return nil, err
		}
		if info.Mode().IsRegular() {
			files = append(files, path)
		}
	}
	sort.Slice(files, func(i, j int) bool { return localeCompare(files[i], files[j]) < 0 })
	return files, nil
}

func readAsset(kind Kind, root, path string, maxBytes int64) (*Asset, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if isSkippableFilesystemError(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		if isSkippableFilesystemError(err) {
			return nil, nil
		}
		return nil, err
	}
	parsed := parseMarkdown(string(contents))
	if strings.TrimSpace(parsed.body) == "" {
		return nil, nil
	}

	name := displayName(path, parsed)
	if kind == Skill {
		folderName := filepath.Base(filepath.Dir(path))
		if !parsed.hasName || !parsed.hasDesc || strings.TrimSpace(parsed.description) == "" ||
			utf16Length(parsed.description) > 1024 || utf16Length(parsed.name) > 64 ||
			!skillNamePattern.MatchString(parsed.name) || parsed.name != folderName {
			return nil, nil
		}
		name = parsed.name
	}
	description := parsed.description
	if !parsed.hasDesc {
		description = firstParagraph(parsed.body)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	asset := &Asset{
		ID:          assetID(kind, path),
		Kind:        kind,
		Name:        name,
		Description: description,
		Source:      filepath.ToSlash(relative),
		Content:     parsed.body,
	}
	if kind == Command {
		word, ok := CommandWord(relative)
		if !ok {
			return nil, nil
		}
		asset.Command, asset.ArgumentHint, asset.TakesArguments = word, parsed.argumentHint, takesArguments(parsed.body)
		asset.CommandOptions, asset.Invalid = commandOptions(parsed)
	}
	return asset, nil
}

func assetID(kind Kind, path string) string {
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = filepath.Clean(absolute)
	}
	hash := sha256.Sum256([]byte(string(kind) + "\x00" + path))
	return hex.EncodeToString(hash[:])
}

func parseMarkdown(markdown string) parsedMarkdown {
	candidate := strings.TrimPrefix(markdown, "\ufeff")
	openingEnd := 0
	switch {
	case strings.HasPrefix(candidate, "---\n"):
		openingEnd = len("---\n")
	case strings.HasPrefix(candidate, "---\r\n"):
		openingEnd = len("---\r\n")
	default:
		return parsedMarkdown{body: strings.TrimSpace(candidate)}
	}

	afterOpening := candidate[openingEnd:]
	lines := strings.Split(afterOpening, "\n")
	metadataLines := make([]string, 0)
	closingOffset := -1
	lineOffset := 0
	for index, line := range lines {
		cleanLine := strings.TrimSuffix(line, "\r")
		if cleanLine == "---" {
			closingOffset = lineOffset + len(line)
			if closingOffset < len(afterOpening) && afterOpening[closingOffset] == '\n' {
				closingOffset++
			}
			break
		}
		metadataLines = append(metadataLines, cleanLine)
		lineOffset += len(line) + 1
		if index == len(lines)-1 {
			lineOffset = len(afterOpening)
		}
	}
	if closingOffset < 0 {
		return parsedMarkdown{body: strings.TrimSpace(candidate)}
	}

	parsed := parsedMarkdown{body: strings.TrimSpace(afterOpening[closingOffset:])}
	for index := 0; index < len(metadataLines); index++ {
		field := frontmatterField.FindStringSubmatch(metadataLines[index])
		if field == nil {
			continue
		}
		key, value := field[1], strings.TrimSpace(field[2])
		if key == "allowed-tools" {
			// A flow list ([read, grep]), a comma-separated line, or a block list of
			// "- read" lines below the key.
			parsed.hasAllowedTools = true
			if value == "" {
				for index+1 < len(metadataLines) && strings.HasPrefix(strings.TrimSpace(metadataLines[index+1]), "-") {
					index++
					parsed.allowedTools = append(parsed.allowedTools, unquoteScalar(strings.TrimPrefix(strings.TrimSpace(metadataLines[index]), "-")))
				}
				continue
			}
			value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
			for _, item := range strings.Split(value, ",") {
				parsed.allowedTools = append(parsed.allowedTools, unquoteScalar(item))
			}
			continue
		}
		if key == "description" && isBlockScalar(value) {
			folded := strings.HasPrefix(value, ">")
			block := make([]string, 0)
			for index+1 < len(metadataLines) && hasLeadingWhitespace(metadataLines[index+1]) {
				index++
				block = append(block, strings.TrimSpace(metadataLines[index]))
			}
			separator := "\n"
			if folded {
				separator = " "
			}
			value = strings.Join(block, separator)
		} else {
			value = unquoteScalar(value)
		}
		if value == "" {
			continue
		}
		switch key {
		case "name":
			parsed.name, parsed.hasName = value, true
		case "title":
			parsed.title, parsed.hasTitle = value, true
		case "description":
			parsed.description, parsed.hasDesc = value, true
		case "argument-hint":
			parsed.argumentHint = value
		case "model":
			parsed.model = value
		case "profile":
			parsed.profile = value
		case "plan":
			parsed.plan = value
		}
	}
	return parsed
}

func isBlockScalar(value string) bool {
	if value == "" || (value[0] != '|' && value[0] != '>') {
		return false
	}
	return len(value) == 1 || (len(value) == 2 && (value[1] == '+' || value[1] == '-'))
}

func hasLeadingWhitespace(value string) bool {
	for _, character := range value {
		return unicode.IsSpace(character)
	}
	return false
}

func unquoteScalar(value string) string {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) {
		if len(trimmed) < 2 {
			return ""
		}
		var parsed string
		if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
			return parsed
		}
		return trimmed[1 : len(trimmed)-1]
	}
	if strings.HasPrefix(trimmed, "'") && strings.HasSuffix(trimmed, "'") {
		if len(trimmed) < 2 {
			return ""
		}
		return strings.ReplaceAll(trimmed[1:len(trimmed)-1], "''", "'")
	}
	return strings.TrimSpace(inlineComment.ReplaceAllString(trimmed, ""))
}

func displayName(path string, parsed parsedMarkdown) string {
	if parsed.hasName {
		return parsed.name
	}
	if parsed.hasTitle {
		return parsed.title
	}
	for _, line := range strings.Split(parsed.body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if heading := firstHeading.FindStringSubmatch(line); heading != nil {
			return strings.TrimSpace(heading[1])
		}
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return separatorSequence.ReplaceAllString(base, " ")
}

func firstParagraph(markdown string) string {
	for _, paragraph := range blankParagraph.Split(markdown, -1) {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph != "" && !strings.HasPrefix(paragraph, "#") {
			return whitespaceRun.ReplaceAllString(paragraph, " ")
		}
	}
	return ""
}

func validKind(kind Kind) bool {
	return kind == Prompt || kind == Template || kind == Skill || kind == Command
}

func directoryList(directories libraryDirectories, kind Kind) []string {
	switch kind {
	case Prompt:
		return directories.prompts
	case Template:
		return directories.templates
	case Skill:
		return directories.skills
	case Command:
		return directories.commands
	default:
		return nil
	}
}

func utf16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func localeCompare(left, right string) int {
	leftLower, rightLower := []rune(strings.ToLower(left)), []rune(strings.ToLower(right))
	limit := len(leftLower)
	if len(rightLower) < limit {
		limit = len(rightLower)
	}
	for index := 0; index < limit; index++ {
		if leftLower[index] < rightLower[index] {
			return -1
		}
		if leftLower[index] > rightLower[index] {
			return 1
		}
	}
	if len(leftLower) < len(rightLower) {
		return -1
	}
	if len(leftLower) > len(rightLower) {
		return 1
	}
	leftRunes, rightRunes := []rune(left), []rune(right)
	for index := 0; index < len(leftRunes) && index < len(rightRunes); index++ {
		if leftRunes[index] == rightRunes[index] {
			continue
		}
		if unicode.ToLower(leftRunes[index]) == unicode.ToLower(rightRunes[index]) {
			if leftRunes[index] == unicode.ToLower(leftRunes[index]) {
				return -1
			}
			if rightRunes[index] == unicode.ToLower(rightRunes[index]) {
				return 1
			}
		}
		if leftRunes[index] < rightRunes[index] {
			return -1
		}
		return 1
	}
	return strings.Compare(left, right)
}
