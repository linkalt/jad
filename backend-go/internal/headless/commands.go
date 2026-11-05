package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"jad/backend-go/internal/contentlibrary"
)

// Slash commands of a headless prompt. They are the web client's "/" menu, reached through
// the same routes the web client calls:
//
//   - /compact, /reset and /state [text] run the session action and end without a turn.
//   - /plan <prompt> runs the prompt in plan mode; the prompt may start with a command
//     of the kinds below.
//   - /prompt:<name>, /template:<name> and /skill:<name> put the library item's text in
//     place of the command word; the text after it follows on a new paragraph.
//   - /mcp:<server>:<prompt> [arg=value ...] [text] puts the MCP prompt's text there.
//   - /resource:<server>:<resource> [variable=value ...] [text] puts the MCP resource's
//     text there; <resource> is its name, the slug of its name, or its URI, and a
//     template's variables are given as name=value.
//   - /<command> [arguments], a custom command of the library (libraries.command_dirs),
//     puts its text there, filled with the arguments ($ARGUMENTS, $1 to $9).
//
// A prompt whose first word is none of these is sent as it is, so "/etc/hosts is wrong"
// is a prompt, not an unknown command; so is a /word that has the shape of a custom
// command when the library has no such command.

// Command kinds.
const (
	CommandCompact  = "compact"
	CommandReset    = "reset"
	CommandState    = "state"
	CommandPlan     = "plan"
	CommandPrompt   = "prompt"
	CommandTemplate = "template"
	CommandSkill    = "skill"
	CommandMCP      = "mcp"
	CommandResource = "resource"
	// CommandCustom is a /word that may name a custom command; Item is the word.
	CommandCustom = "custom"
)

// errNotACommand is a custom-command word the library does not have: the input is a
// prompt after all.
var errNotACommand = errors.New("not a command")

// Command is the slash command a prompt starts with.
type Command struct {
	// Kind is one of the Command* constants.
	Kind string
	// Word is the command word without its slash, as typed ("prompt:review").
	Word string
	// Item is the library item's slug, or the MCP prompt's name.
	Item string
	// Server is the MCP server of an mcp command.
	Server string
	// Text is what follows the command word, without the space around it.
	Text string
}

var mcpServerPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ParseCommand reads the slash command the input starts with. It returns false for an
// input that does not start with one.
func ParseCommand(input string) (Command, bool) {
	trimmed := strings.TrimLeftFunc(input, unicode.IsSpace)
	if !strings.HasPrefix(trimmed, "/") {
		return Command{}, false
	}
	word, rest := trimmed, ""
	if end := strings.IndexFunc(trimmed, unicode.IsSpace); end >= 0 {
		word, rest = trimmed[:end], trimmed[end:]
	}
	command := Command{Word: word[1:], Text: strings.TrimSpace(rest)}
	switch command.Word {
	case CommandCompact, CommandReset, CommandState, CommandPlan:
		command.Kind = command.Word
		return command, true
	}
	kind, item, found := strings.Cut(command.Word, ":")
	if !found || item == "" {
		return customCommand(command)
	}
	switch kind {
	case CommandPrompt, CommandTemplate, CommandSkill:
		command.Kind, command.Item = kind, item
		return command, true
	case CommandMCP, CommandResource:
		server, name, found := strings.Cut(item, ":")
		if !found || name == "" || !mcpServerPattern.MatchString(server) {
			return Command{}, false
		}
		command.Kind, command.Server, command.Item = kind, server, name
		return command, true
	}
	return customCommand(command)
}

// customCommand reads a word that has the shape of a custom command's: slugs joined by
// ":", none of them a word another kind owns.
func customCommand(command Command) (Command, bool) {
	if command.Word == "" || contentlibrary.IsReservedCommandWord(command.Word) {
		return Command{}, false
	}
	for _, part := range strings.Split(command.Word, ":") {
		if part == "" || contentlibrary.Slug(part) != part {
			return Command{}, false
		}
	}
	command.Kind, command.Item = CommandCustom, command.Word
	return command, true
}

// IsAction reports whether the command is a session action, which ends without a turn.
func (c Command) IsAction() bool {
	return c.Kind == CommandCompact || c.Kind == CommandReset || c.Kind == CommandState
}

// Check refuses what the command cannot take: text after /compact or /reset, a model, an
// output schema or plan mode for an action, which starts no turn, and a /plan with no
// prompt or followed by an action.
func (c Command) Check(model string, output, plan bool) error {
	if c.Kind == CommandPlan {
		if c.Text == "" {
			return errors.New("/plan needs a prompt after it")
		}
		if inner, ok := ParseCommand(c.Text); ok {
			if inner.IsAction() || inner.Kind == CommandPlan {
				return fmt.Errorf("/plan cannot be followed by /%s", inner.Word)
			}
			return inner.Check(model, output, true)
		}
		return nil
	}
	if c.IsAction() && plan {
		return fmt.Errorf("/%s starts no turn, so it cannot run in plan mode", c.Kind)
	}
	if (c.Kind == CommandCompact || c.Kind == CommandReset) && c.Text != "" {
		return fmt.Errorf("/%s takes no text", c.Kind)
	}
	if c.IsAction() && model != "" {
		return fmt.Errorf("/%s starts no turn, so it takes no model", c.Kind)
	}
	if c.IsAction() && output {
		return fmt.Errorf("/%s starts no turn, so it takes no output schema", c.Kind)
	}
	return nil
}

// runAction runs /compact, /reset or /state and returns its result.
func (r *Runner) runAction(ctx context.Context, request Request, command Command) (Result, error) {
	var path string
	var body any
	switch command.Kind {
	case CommandCompact:
		path = "compact"
	case CommandReset:
		path = "reset"
	case CommandState:
		path, body = "statecontext", map[string]any{"prompt": command.Text, "preview": false}
	}
	raw, err := r.call(ctx, http.MethodPost, r.sessionPath(request.Session, path, request.Profile), body)
	if err != nil {
		return Result{}, err
	}
	result := Result{Status: StatusCompleted, Session: request.Session, Profile: request.Profile, Command: command.Word, CommandResult: raw}
	switch command.Kind {
	case CommandCompact:
		var answer struct {
			Compaction struct {
				Compacted      bool `json:"compacted"`
				CompactedItems int  `json:"compacted_items"`
				RetainedItems  int  `json:"retained_items"`
			} `json:"compaction"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return Result{}, fmt.Errorf("read the compaction result: %w", err)
		}
		if answer.Compaction.Compacted {
			result.Text = fmt.Sprintf("Compacted %d items; %d kept.", answer.Compaction.CompactedItems, answer.Compaction.RetainedItems)
		} else {
			result.Text = fmt.Sprintf("Nothing to compact; %d items kept.", answer.Compaction.RetainedItems)
		}
		if len(answer.Usage) > 0 {
			result.Usage = answer.Usage
		}
	case CommandReset:
		result.Text = "The session was reset."
	case CommandState:
		var answer struct {
			State struct {
				Written bool   `json:"written"`
				Role    string `json:"role"`
				Bytes   int    `json:"bytes"`
			} `json:"statecontext"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return Result{}, fmt.Errorf("read the state-context result: %w", err)
		}
		if answer.State.Written {
			result.Text = fmt.Sprintf("Wrote a %s state-context block (%d bytes).", answer.State.Role, answer.State.Bytes)
		} else {
			result.Text = "No state-context block was written."
		}
	}
	return result, nil
}

// expand returns the text a library or MCP command stands for, followed by the command's
// text on a new paragraph.
func (r *Runner) expand(ctx context.Context, request Request, command Command) (string, commandOptions, error) {
	var text, rest string
	var options commandOptions
	var err error
	switch command.Kind {
	case CommandMCP:
		text, rest, err = r.mcpPrompt(ctx, request, command)
	case CommandResource:
		text, rest, err = r.mcpResource(ctx, request, command, &options.attachments)
	case CommandCustom:
		// The arguments are part of the command's text, not a paragraph after it.
		text, options, err = r.customCommandText(ctx, command)
	default:
		text, err = r.libraryItem(ctx, command)
		rest = command.Text
	}
	if err != nil {
		return "", commandOptions{}, err
	}
	if rest == "" {
		return text, options, nil
	}
	if strings.TrimSpace(text) == "" {
		return rest, options, nil
	}
	return strings.TrimRight(text, "\n") + "\n\n" + rest, options, nil
}

// commandOptions are the run options of a custom command's front matter, as the expand
// route gives them.
type commandOptions struct {
	Model        string   `json:"model"`
	Profile      string   `json:"profile"`
	Plan         bool     `json:"plan"`
	AllowedTools []string `json:"allowed_tools"`
	// attachments are the images and PDFs of a /resource command's resource.
	attachments []Attachment
}

// apply sets what the request does not set itself: a --model, --profile or tool list the
// caller gave wins over the command's. plan: true turns plan mode on.
func (o commandOptions) apply(request *Request) {
	if request.Model == "" {
		request.Model = o.Model
	}
	if request.Profile == "" {
		request.Profile = o.Profile
	}
	if o.Plan {
		request.Plan = true
	}
	if request.Tools == nil && o.AllowedTools != nil {
		request.Tools = o.AllowedTools
	}
}

// customCommandText finds the custom command named by the word and returns its text
// filled with the command's text as arguments, as the expand route gives it. A word the
// library has no command for is errNotACommand, unless the library could not list its
// commands (commands_error): the word may then be a command that is not listed, so it is
// refused with the reason instead of being sent to the model as a prompt.
func (r *Runner) customCommandText(ctx context.Context, command Command) (string, commandOptions, error) {
	raw, err := r.call(ctx, http.MethodGet, "/api/library", nil)
	if err != nil {
		return "", commandOptions{}, err
	}
	var catalog struct {
		Commands []struct {
			ID      string `json:"id"`
			Command string `json:"command"`
		} `json:"commands"`
		CommandsError string `json:"commands_error"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return "", commandOptions{}, fmt.Errorf("read the library: %w", err)
	}
	for _, entry := range catalog.Commands {
		if entry.Command != command.Item {
			continue
		}
		raw, err := r.call(ctx, http.MethodPost, "/api/library/command/"+url.PathEscape(entry.ID)+"/expand", map[string]string{"arguments": command.Text})
		if err != nil {
			return "", commandOptions{}, err
		}
		var expanded struct {
			Text string `json:"text"`
			commandOptions
		}
		if err := json.Unmarshal(raw, &expanded); err != nil {
			return "", commandOptions{}, fmt.Errorf("read the command's text: %w", err)
		}
		return expanded.Text, expanded.commandOptions, nil
	}
	if catalog.CommandsError != "" {
		return "", commandOptions{}, fmt.Errorf("/%s: the library's commands could not be listed: %s (use --no-commands to send the text as a prompt)", command.Item, catalog.CommandsError)
	}
	return "", commandOptions{}, errNotACommand
}

// libraryItem reads the library item a /prompt:, /template: or /skill: command names. The
// name is the web client's slug: the item's name in lower case, every run of characters
// that are not letters or digits made one dash, with -2, -3 ... after a name taken earlier
// in the library's order.
func (r *Runner) libraryItem(ctx context.Context, command Command) (string, error) {
	raw, err := r.call(ctx, http.MethodGet, "/api/library", nil)
	if err != nil {
		return "", err
	}
	var catalog struct {
		Prompts   []librarySummary `json:"prompts"`
		Templates []librarySummary `json:"templates"`
		Skills    []librarySummary `json:"skills"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return "", fmt.Errorf("read the library: %w", err)
	}
	keys := libraryKeys([][]librarySummary{catalog.Prompts, catalog.Templates, catalog.Skills})
	want := command.Kind + ":" + command.Item
	var known []string
	for _, entry := range keys {
		if entry.summary.Kind != command.Kind {
			continue
		}
		if entry.key == want {
			raw, err := r.call(ctx, http.MethodGet, "/api/library/"+url.PathEscape(entry.summary.Kind)+"/"+url.PathEscape(entry.summary.ID), nil)
			if err != nil {
				return "", err
			}
			var item struct {
				Asset struct {
					Content string `json:"content"`
				} `json:"asset"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return "", fmt.Errorf("read the library item: %w", err)
			}
			return item.Asset.Content, nil
		}
		known = append(known, "/"+entry.key)
	}
	if len(known) == 0 {
		return "", fmt.Errorf("no /%s in the library: it has no %s items", want, command.Kind)
	}
	return "", fmt.Errorf("no /%s in the library; its %s items are %s", want, command.Kind, strings.Join(known, ", "))
}

type librarySummary struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type libraryKey struct {
	key     string
	summary librarySummary
}

// libraryKeys gives each item its menu key, kind:slug, in the order the web client does.
func libraryKeys(groups [][]librarySummary) []libraryKey {
	taken := map[string]bool{CommandCompact: true, CommandState: true, CommandReset: true, CommandPlan: true}
	var keys []libraryKey
	for _, group := range groups {
		for _, summary := range group {
			base := summary.Kind + ":" + LibrarySlug(summary.Name)
			key := base
			for suffix := 2; taken[key]; suffix++ {
				key = fmt.Sprintf("%s-%d", base, suffix)
			}
			taken[key] = true
			keys = append(keys, libraryKey{key: key, summary: summary})
		}
	}
	return keys
}

// LibrarySlug is the name a person types after the kind: the name in lower case, with every
// run of characters that are not letters or digits made one dash and the dashes at either
// end dropped; "untitled" when nothing is left. It is the web client's librarySlug.
func LibrarySlug(name string) string {
	return contentlibrary.Slug(name)
}

type mcpPromptArgument struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

type mcpPromptEntry struct {
	Server    string              `json:"server"`
	Name      string              `json:"name"`
	Arguments []mcpPromptArgument `json:"arguments"`
}

// mcpPrompt fills the MCP prompt an /mcp:<server>:<prompt> command names. The prompt is
// found by its name, or by the slug of its name as the web client's menu shows it. The
// command's text starts with the arguments as name=value words (a value may be in double
// quotes, with \" and \\ inside); the first word that is not one ends them, and the rest
// follows the prompt. A prompt with one argument and no name=value word takes the whole
// text as that argument.
func (r *Runner) mcpPrompt(ctx context.Context, request Request, command Command) (string, string, error) {
	raw, err := r.call(ctx, http.MethodGet, r.sessionPath(request.Session, "mcp-prompts", request.Profile), nil)
	if err != nil {
		return "", "", err
	}
	var listed struct {
		Prompts  []mcpPromptEntry `json:"prompts"`
		Failures []struct {
			Server string `json:"server"`
			Error  string `json:"error"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return "", "", fmt.Errorf("read the MCP prompts: %w", err)
	}
	for _, failure := range listed.Failures {
		if failure.Server == command.Server {
			return "", "", fmt.Errorf("list the prompts of MCP server %s: %s", failure.Server, failure.Error)
		}
	}
	var prompt *mcpPromptEntry
	var names []string
	bySlug := []int{}
	for index := range listed.Prompts {
		entry := &listed.Prompts[index]
		if entry.Server != command.Server {
			continue
		}
		names = append(names, entry.Name)
		if entry.Name == command.Item {
			prompt = entry
		} else if LibrarySlug(entry.Name) == command.Item {
			bySlug = append(bySlug, index)
		}
	}
	if prompt == nil && len(bySlug) == 1 {
		prompt = &listed.Prompts[bySlug[0]]
	}
	if prompt == nil {
		switch {
		case len(bySlug) > 1:
			return "", "", fmt.Errorf("MCP server %s has more than one prompt named like %q; use the exact name", command.Server, command.Item)
		case len(names) == 0:
			return "", "", fmt.Errorf("no MCP prompts of server %s: the profile's toolsets must list it with prompts: true", command.Server)
		}
		sort.Strings(names)
		return "", "", fmt.Errorf("MCP server %s has no prompt %q; its prompts are %s", command.Server, command.Item, strings.Join(names, ", "))
	}
	arguments, rest, err := promptArguments(*prompt, command.Text)
	if err != nil {
		return "", "", err
	}
	raw, err = r.call(ctx, http.MethodPost, r.sessionPath(request.Session, "mcp-prompts/get", request.Profile), map[string]any{
		"server": prompt.Server, "name": prompt.Name, "arguments": arguments,
	})
	if err != nil {
		return "", "", err
	}
	var filled struct {
		Prompt struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &filled); err != nil {
		return "", "", fmt.Errorf("read the MCP prompt: %w", err)
	}
	return filled.Prompt.Text, rest, nil
}

type mcpResourceEntry struct {
	Server      string   `json:"server"`
	URI         string   `json:"uri"`
	URITemplate string   `json:"uri_template"`
	Variables   []string `json:"variables"`
	Name        string   `json:"name"`
}

// mcpResource reads the MCP resource a /resource:<server>:<resource> command names: by
// its name, the slug of its name, or its URI, among the server's resources and templates.
// A template's variables are name=value words at the start of the text, as a prompt's
// arguments are; the rest of the text follows the resource. A URI the server does not
// list (a resource it can read but does not list) is read as given.
func (r *Runner) mcpResource(ctx context.Context, request Request, command Command, attachments *[]Attachment) (string, string, error) {
	raw, err := r.call(ctx, http.MethodGet, r.sessionPath(request.Session, "mcp-resources", request.Profile), nil)
	if err != nil {
		return "", "", err
	}
	var listed struct {
		Resources []mcpResourceEntry `json:"resources"`
		Failures  []struct {
			Server string `json:"server"`
			Error  string `json:"error"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return "", "", fmt.Errorf("read the MCP resources: %w", err)
	}
	for _, failure := range listed.Failures {
		if failure.Server == command.Server {
			return "", "", fmt.Errorf("list the resources of MCP server %s: %s", failure.Server, failure.Error)
		}
	}
	var found *mcpResourceEntry
	var names []string
	var bySlug []int
	listedServer := false
	for index := range listed.Resources {
		entry := &listed.Resources[index]
		if entry.Server != command.Server {
			continue
		}
		listedServer = true
		names = append(names, entry.Name)
		switch {
		case entry.Name == command.Item || (entry.URI != "" && entry.URI == command.Item):
			if found == nil {
				found = entry
			}
		case LibrarySlug(entry.Name) == command.Item:
			bySlug = append(bySlug, index)
		}
	}
	if found == nil && len(bySlug) == 1 {
		found = &listed.Resources[bySlug[0]]
	}
	body := map[string]any{"server": command.Server}
	rest := command.Text
	switch {
	case found != nil && found.URITemplate != "":
		declared := map[string]bool{}
		for _, variable := range found.Variables {
			declared[variable] = true
		}
		variables := map[string]string{}
		for {
			rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
			name, value, remaining, ok := namedArgument(rest, declared)
			if !ok {
				break
			}
			if _, repeated := variables[name]; repeated {
				return "", "", fmt.Errorf("variable %s of MCP resource %s is given twice", name, found.Name)
			}
			variables[name] = value
			rest = remaining
		}
		if len(variables) == 0 && len(found.Variables) > 0 {
			return "", "", fmt.Errorf("MCP resource template %s needs its variables as name=value after the command (it takes %s)", found.Name, strings.Join(found.Variables, ", "))
		}
		body["uri_template"], body["variables"] = found.URITemplate, variables
	case found != nil:
		body["uri"] = found.URI
	case len(bySlug) > 1:
		return "", "", fmt.Errorf("MCP server %s has more than one resource named like %q; use the exact name or the URI", command.Server, command.Item)
	case strings.Contains(command.Item, "://"):
		body["uri"] = command.Item
	case !listedServer:
		return "", "", fmt.Errorf("MCP server %s lists no resources: the profile's toolsets must list it with resources: true", command.Server)
	default:
		sort.Strings(names)
		return "", "", fmt.Errorf("MCP server %s has no resource %q; its resources are %s", command.Server, command.Item, strings.Join(names, ", "))
	}
	raw, err = r.call(ctx, http.MethodPost, r.sessionPath(request.Session, "mcp-resources/read", request.Profile), body)
	if err != nil {
		return "", "", err
	}
	var read struct {
		Resource struct {
			Text        string       `json:"text"`
			Attachments []Attachment `json:"attachments"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(raw, &read); err != nil {
		return "", "", fmt.Errorf("read the MCP resource: %w", err)
	}
	*attachments = append(*attachments, read.Resource.Attachments...)
	return read.Resource.Text, strings.TrimSpace(rest), nil
}

// promptArguments splits a command's text into the prompt's arguments and the rest.
func promptArguments(prompt mcpPromptEntry, text string) (map[string]string, string, error) {
	declared := make(map[string]bool, len(prompt.Arguments))
	for _, argument := range prompt.Arguments {
		declared[argument.Name] = true
	}
	arguments := map[string]string{}
	rest := text
	for {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		name, value, remaining, ok := namedArgument(rest, declared)
		if !ok {
			break
		}
		if _, repeated := arguments[name]; repeated {
			return nil, "", fmt.Errorf("argument %s of MCP prompt %s is given twice", name, prompt.Name)
		}
		arguments[name] = value
		rest = remaining
	}
	rest = strings.TrimSpace(rest)
	if len(arguments) == 0 && len(prompt.Arguments) == 1 && rest != "" {
		arguments[prompt.Arguments[0].Name] = rest
		rest = ""
	}
	var missing []string
	for _, argument := range prompt.Arguments {
		if _, given := arguments[argument.Name]; argument.Required && !given {
			missing = append(missing, argument.Name)
		}
	}
	if len(missing) > 0 {
		all := make([]string, 0, len(prompt.Arguments))
		for _, argument := range prompt.Arguments {
			all = append(all, argument.Name)
		}
		return nil, "", fmt.Errorf("MCP prompt %s needs %s; give each as name=value after the command (it takes %s)", prompt.Name, strings.Join(missing, ", "), strings.Join(all, ", "))
	}
	return arguments, rest, nil
}

// namedArgument reads one name=value word at the start of text, for a declared name.
func namedArgument(text string, declared map[string]bool) (string, string, string, bool) {
	equals := strings.IndexByte(text, '=')
	if equals <= 0 || strings.IndexFunc(text[:equals], unicode.IsSpace) >= 0 || !declared[text[:equals]] {
		return "", "", "", false
	}
	name, value := text[:equals], text[equals+1:]
	if !strings.HasPrefix(value, `"`) {
		end := strings.IndexFunc(value, unicode.IsSpace)
		if end < 0 {
			return name, value, "", true
		}
		return name, value[:end], value[end:], true
	}
	var builder strings.Builder
	for index := 1; index < len(value); index++ {
		switch value[index] {
		case '\\':
			if index+1 < len(value) && (value[index+1] == '"' || value[index+1] == '\\') {
				index++
			}
			builder.WriteByte(value[index])
		case '"':
			return name, builder.String(), value[index+1:], true
		default:
			builder.WriteByte(value[index])
		}
	}
	// An unclosed quote is not an argument; the word is left in the text.
	return "", "", "", false
}

// call sends one request to the handler and returns the body of a 2xx answer. Any other
// answer is a RequestError with the server's message.
func (r *Runner) call(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	// A server gives a handler an empty body, never a nil one.
	var reader io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(encoded))
	} else if method == http.MethodPost {
		reader = strings.NewReader("{}")
	}
	request, err := http.NewRequestWithContext(ctx, method, path, reader)
	if err != nil {
		return nil, err
	}
	r.authorize(request)
	recorder := newRecordingWriter()
	r.handler.ServeHTTP(recorder, request)
	if recorder.status < 200 || recorder.status > 299 {
		return nil, &RequestError{Status: recorder.status, Message: errorMessage(recorder.body.Bytes())}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.RawMessage(recorder.body.Bytes()), nil
}

// recordingWriter keeps a synchronous answer.
type recordingWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{header: http.Header{}}
}

func (w *recordingWriter) Header() http.Header { return w.header }

func (w *recordingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *recordingWriter) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(data)
}

// runCommand runs the slash command a request starts with. An action ends the request
// with its result; a library or MCP command puts its text in request.Input and returns
// done false. A context that ends while the command runs gives an aborted or timed-out
// result, as a run would.
func (r *Runner) runCommand(ctx context.Context, request *Request, command Command) (Result, bool, error) {
	started := time.Now()
	commandCtx, cancel := ctx, context.CancelFunc(func() {})
	if request.Timeout > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, request.Timeout)
	}
	defer cancel()
	stopped := func(err error) (Result, bool, error) {
		result := Result{Session: request.Session, Profile: request.Profile, Command: command.Word, Error: err.Error(), DurationMS: time.Since(started).Milliseconds()}
		switch {
		case ctx.Err() != nil:
			result.Status = StatusAborted
		case errors.Is(commandCtx.Err(), context.DeadlineExceeded):
			result.Status = StatusTimedOut
		default:
			return Result{}, false, err
		}
		return result, true, nil
	}
	if command.IsAction() {
		result, err := r.runAction(commandCtx, *request, command)
		if err != nil {
			return stopped(err)
		}
		result.DurationMS = time.Since(started).Milliseconds()
		return result, true, nil
	}
	input, options, err := r.expand(commandCtx, *request, command)
	if err != nil {
		return stopped(err)
	}
	if strings.TrimSpace(input) == "" && len(options.attachments) == 0 {
		return Result{}, false, fmt.Errorf("/%s stands for an empty text", command.Word)
	}
	request.Input = input
	request.Attachments = append(request.Attachments, options.attachments...)
	options.apply(request)
	if request.Timeout > 0 {
		// The run has what the command left of the timeout.
		request.Timeout -= time.Since(started)
		if request.Timeout <= 0 {
			return stopped(context.DeadlineExceeded)
		}
	}
	return Result{}, false, nil
}
