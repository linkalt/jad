package contentlibrary

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandWord(t *testing.T) {
	for relative, want := range map[string]string{
		"review.md":          "review",
		"git/Commit All.md":  "git:commit-all",
		"Deploy/Prod/Now.MD": "deploy:prod:now",
		"compact.md":         "",
		"plan.md":            "",
		"prompt/x.md":        "",
		"mcp/server/x.md":    "",
		"resource/x.md":      "",
		"prompt.md":          "prompt",
		"skills/x.md":        "skills:x",
	} {
		word, ok := CommandWord(relative)
		if (want == "") == ok || word != want {
			t.Errorf("CommandWord(%q) = %q %v, want %q", relative, word, ok, want)
		}
	}
	for word, reserved := range map[string]bool{"reset": true, "plan": true, "skill:x": true, "resource:a:b": true, "skill": false, "deploy:x": false} {
		if IsReservedCommandWord(word) != reserved {
			t.Errorf("IsReservedCommandWord(%q) = %v", word, !reserved)
		}
	}
}

// The same cases as frontend_new/src/slash-commands.test.mjs.
func TestSlugMatchesTheWebClient(t *testing.T) {
	for name, want := range map[string]string{"Software engineering": "software-engineering", "  A -- B / C!  ": "a-b-c", "日本語 テスト": "日本語-テスト", "***": "untitled"} {
		if got := Slug(name); got != want {
			t.Errorf("Slug(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestExpandCommand(t *testing.T) {
	for _, tc := range []struct{ text, arguments, want string }{
		{"Review the code.", "", "Review the code."},
		{"Review the code.\n", "main.go", "Review the code.\n\nmain.go"},
		{"Fix issue $ARGUMENTS now.", "  #12 and #13 ", "Fix issue #12 and #13 now."},
		{"From $1 to $2, then $3.", `a "b c" d`, "From a to b c, then d."},
		{"Only $1 and $9.", "one", "Only one and ."},
		{"Quoted $1", `"say \"hi\" \\ bye"`, `Quoted say "hi" \ bye`},
		{"All: $ARGUMENTS; first: $1", "x y", "All: x y; first: x"},
		{"Price $5 is not $10", "a b c d e", "Price e is not a0"},
		{"No args $ARGUMENTS.", "", "No args ."},
	} {
		if got := ExpandCommand(tc.text, tc.arguments); got != tc.want {
			t.Errorf("ExpandCommand(%q, %q) = %q, want %q", tc.text, tc.arguments, got, tc.want)
		}
	}
}

func TestLibraryListsCommandsFromEachFolderInOrder(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"command_dirs":["first","second"]}}`)
	writeLibraryTestFile(t, filepath.Join(root, "first/review.md"), "---\ndescription: Review a file\nargument-hint: <file>\n---\nReview $ARGUMENTS carefully.\n")
	writeLibraryTestFile(t, filepath.Join(root, "first/git/commit.md"), "# Commit\n\nCommit the staged changes.\n")
	writeLibraryTestFile(t, filepath.Join(root, "second/review.md"), "Hidden by the first folder.\n")
	writeLibraryTestFile(t, filepath.Join(root, "second/deploy.md"), "Deploy to $1.\n")
	writeLibraryTestFile(t, filepath.Join(root, "second/reset.md"), "A built-in's word.\n")
	writeLibraryTestFile(t, filepath.Join(root, "second/prompt/x.md"), "Another kind's word.\n")
	writeLibraryTestFile(t, filepath.Join(root, "second/empty.md"), "---\ndescription: nothing\n---\n")

	library := New(Options{WorkingDir: root, Environment: map[string]string{}})
	catalog, err := library.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var words []string
	for _, command := range catalog.Commands {
		words = append(words, command.Command)
	}
	if strings.Join(words, ",") != "deploy,git:commit,review" {
		t.Fatalf("commands = %v", words)
	}
	review := catalog.Commands[2]
	if review.Kind != Command || review.Source != "review.md" || review.Description != "Review a file" || review.ArgumentHint != "<file>" || !review.TakesArguments {
		t.Fatalf("review = %+v", review)
	}
	if commit := catalog.Commands[1]; commit.Name != "Commit" || commit.TakesArguments || commit.Source != "git/commit.md" {
		t.Fatalf("commit = %+v", commit)
	}
	if len(catalog.Prompts) != 0 {
		t.Fatalf("a command was listed as a prompt: %+v", catalog.Prompts)
	}
	asset, found, err := library.Read(context.Background(), Command, review.ID)
	if err != nil || !found || asset.Content != "Review $ARGUMENTS carefully." || asset.Command != "review" {
		t.Fatalf("read = %+v %v %v", asset, found, err)
	}
	encoded, _ := json.Marshal(catalog.Commands[1])
	if strings.Contains(string(encoded), "argument_hint") || strings.Contains(string(encoded), "takes_arguments") {
		t.Fatalf("empty command fields were sent: %s", encoded)
	}
	encoded, _ = json.Marshal(review)
	if !strings.Contains(string(encoded), "\"command\":\"review\",\"argument_hint\":\"\\u003cfile\\u003e\",\"takes_arguments\":true") {
		t.Fatalf("command fields: %s", encoded)
	}
}

// A command folder that is a file is reported in commands_error and costs only the
// commands: the prompts beside it are still listed.
func TestCommandFolderThatIsAFileIsReportedWithoutFailingTheList(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"command_dirs":["file"],"prompt_dirs":["prompts"]}}`)
	writeLibraryTestFile(t, filepath.Join(root, "prompts", "hello.md"), "Say hello.")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := New(Options{WorkingDir: root, Environment: map[string]string{}}).List(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(catalog.CommandsError, "for command is not a directory") || catalog.Commands == nil || len(catalog.Commands) != 0 {
		t.Fatalf("commands = %+v, error %q", catalog.Commands, catalog.CommandsError)
	}
	if len(catalog.Prompts) != 1 || catalog.Prompts[0].Name != "hello" {
		t.Fatalf("prompts = %+v", catalog.Prompts)
	}
}

// The default command folder, .agent/commands, being a file is the same case.
func TestDefaultCommandFolderThatIsAFileKeepsTheOtherKinds(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, ".agent", "prompts", "hello.md"), "Say hello.")
	writeLibraryTestFile(t, filepath.Join(root, ".agent", "commands"), "not a folder")
	catalog, err := New(Options{WorkingDir: root, Environment: map[string]string{}}).List(context.Background())
	if err != nil || len(catalog.Prompts) != 1 || catalog.CommandsError == "" {
		t.Fatalf("catalog = %+v, err %v", catalog, err)
	}
	encoded, _ := json.Marshal(catalog)
	if !strings.Contains(string(encoded), `"commands":[],"commands_error":`) {
		t.Fatalf("json = %s", encoded)
	}
}

// Without a problem there is no commands_error in the JSON.
func TestCatalogWithoutCommandProblemHasNoCommandsError(t *testing.T) {
	catalog, err := New(Options{WorkingDir: t.TempDir(), Environment: map[string]string{}}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(catalog); strings.Contains(string(encoded), "commands_error") {
		t.Fatalf("json = %s", encoded)
	}
}

func TestDefaultDirectoriesIncludeCommands(t *testing.T) {
	defaults, err := DefaultDirectories()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(defaults["command_dirs"], ",") != ".agent/commands" {
		t.Fatalf("command_dirs default = %v", defaults["command_dirs"])
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", "root-setting.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Libraries struct {
				Properties map[string]struct {
					Default json.RawMessage `json:"default"`
				} `json:"properties"`
			} `json:"libraries"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	for key := range defaults {
		if _, described := schema.Properties.Libraries.Properties[key]; !described {
			t.Errorf("root-setting.schema.json has no libraries.%s", key)
		}
	}
	var got []string
	if err := json.Unmarshal(schema.Properties.Libraries.Properties["command_dirs"].Default, &got); err != nil || strings.Join(got, ",") != strings.Join(defaults["command_dirs"], ",") {
		t.Errorf("schema default %v, loader default %v", got, defaults["command_dirs"])
	}
}

// A command's front matter may ask for a model, a profile, plan mode and a narrower tool
// list; a bad value is reported on the command, which is still listed.
func TestCommandRunOptions(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"command_dirs":["commands"]}}`)
	for name, text := range map[string]string{
		"flow.md":     "---\nmodel: openai/fast@high\nprofile: dev/coder\nplan: true\nallowed-tools: [read, \"grep\", read]\n---\nPlan it.\n",
		"comma.md":    "---\nallowed-tools: read, mcp:docs:*\n---\nRead it.\n",
		"block.md":    "---\nallowed-tools:\n  - glob\n- 'mcp:docs:search'\nplan: false\n---\nFind it.\n",
		"plain.md":    "Just text.\n",
		"badplan.md":  "---\nplan: yes\n---\nx\n",
		"badtool.md":  "---\nallowed-tools: Bash\n---\nx\n",
		"notools.md":  "---\nallowed-tools: []\n---\nx\n",
		"badprof.md":  "---\nprofile: a/b/c\n---\nx\n",
		"badmodel.md": "---\nmodel: \"a b\"\n---\nx\n",
	} {
		writeLibraryTestFile(t, filepath.Join(root, "commands", name), text)
	}
	catalog, err := New(Options{WorkingDir: root, Environment: map[string]string{}}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byWord := map[string]Summary{}
	for _, command := range catalog.Commands {
		byWord[command.Command] = command
	}
	want := map[string]CommandOptions{
		"flow":  {Model: "openai/fast@high", Profile: "dev/coder", Plan: true, AllowedTools: []string{"read", "grep"}},
		"comma": {AllowedTools: []string{"read", "mcp:docs:*"}},
		"block": {AllowedTools: []string{"glob", "mcp:docs:search"}},
		"plain": {},
	}
	for word, options := range want {
		got := byWord[word]
		if got.Invalid != "" || got.Model != options.Model || got.Profile != options.Profile || got.Plan != options.Plan || strings.Join(got.AllowedTools, ",") != strings.Join(options.AllowedTools, ",") {
			t.Errorf("/%s options = %+v, %q; want %+v", word, got.CommandOptions, got.Invalid, options)
		}
	}
	for word, problem := range map[string]string{
		"badplan":  `plan must be true or false, not "yes"`,
		"badtool":  `allowed-tools: "Bash" is not a native tool`,
		"notools":  "allowed-tools lists no tool",
		"badprof":  `profile "a/b/c" must be <category>/<name>`,
		"badmodel": `model "a b" must not contain white space`,
	} {
		if got := byWord[word]; !strings.Contains(got.Invalid, problem) || got.Plan || got.AllowedTools != nil {
			t.Errorf("/%s: invalid %q, options %+v; want %q", word, got.Invalid, got.CommandOptions, problem)
		}
	}
	encoded, _ := json.Marshal(byWord["plain"])
	if strings.Contains(string(encoded), "model") || strings.Contains(string(encoded), "allowed_tools") || strings.Contains(string(encoded), "invalid") || strings.Contains(string(encoded), "plan") {
		t.Fatalf("empty run options were sent: %s", encoded)
	}
	encoded, _ = json.Marshal(byWord["flow"])
	if !strings.Contains(string(encoded), `"model":"openai/fast@high","profile":"dev/coder","plan":true,"allowed_tools":["read","grep"]`) {
		t.Fatalf("run options: %s", encoded)
	}

	// CheckCommands reports the first problem of a folder and counts the good ones.
	if count, err := CheckCommands(context.Background(), root, "commands", 1<<20); err == nil || count != 0 {
		t.Fatalf("CheckCommands over bad commands = %d, %v", count, err)
	}
	for _, name := range []string{"badplan.md", "badtool.md", "notools.md", "badprof.md", "badmodel.md"} {
		if err := os.Remove(filepath.Join(root, "commands", name)); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := CheckCommands(context.Background(), root, "commands", 1<<20); err != nil || count != 4 {
		t.Fatalf("CheckCommands = %d, %v", count, err)
	}
	if _, err := CheckCommands(context.Background(), root, "commands", 10); err == nil || !strings.Contains(err.Error(), "more than libraries.max_asset_bytes") {
		t.Fatalf("CheckCommands over the size limit = %v", err)
	}
}
