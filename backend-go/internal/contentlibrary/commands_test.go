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

func TestCommandFolderThatIsAFileIsAConfigError(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"command_dirs":["file"]}}`)
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(Options{WorkingDir: root, Environment: map[string]string{}}).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "for command is not a directory") {
		t.Fatalf("err = %v", err)
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
