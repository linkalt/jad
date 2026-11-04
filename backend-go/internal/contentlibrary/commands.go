package contentlibrary

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Custom slash commands. A command is a Markdown file in one of libraries.command_dirs;
// its word is its path below that folder without .md, each part made a Slug and the
// parts joined by ":" (review.md is /review, git/commit.md is /git:commit). The text may
// hold $ARGUMENTS, all the text typed after the word, and $1 to $9, its words (a double-
// quoted run is one word). A text with neither gets the arguments on a new paragraph.

// reservedCommandWords are the built-in commands; a command file cannot take their word.
var reservedCommandWords = []string{"compact", "state", "reset", "plan"}

// reservedCommandPrefixes are the kinds of the other "/" words (/prompt:<name>, ...); a
// command whose first part is one of them would read as that kind, so it is skipped.
var reservedCommandPrefixes = []string{"prompt", "template", "skill", "mcp", "resource"}

var commandPlaceholder = regexp.MustCompile(`\$(ARGUMENTS|[1-9])`)

// Slug is the name a person types after the slash: the name in lower case, every run of
// characters that are not letters or digits made one dash, the dashes at either end
// dropped; "untitled" when nothing is left. It is the web client's librarySlug.
func Slug(name string) string {
	var builder strings.Builder
	dash := false
	for _, character := range strings.ToLower(name) {
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			if dash && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			dash = false
			builder.WriteRune(character)
			continue
		}
		dash = true
	}
	if builder.Len() == 0 {
		return "untitled"
	}
	return builder.String()
}

// CommandWord is the word of the command file at relative, a path below its command
// folder. It is false for a word a built-in command or another kind of "/" word owns.
func CommandWord(relative string) (string, bool) {
	relative = strings.TrimSuffix(filepath.ToSlash(relative), filepath.Ext(relative))
	parts := strings.Split(relative, "/")
	for index, part := range parts {
		parts[index] = Slug(part)
	}
	word := strings.Join(parts, ":")
	if containsWord(reservedCommandWords, word) || (len(parts) > 1 && containsWord(reservedCommandPrefixes, parts[0])) {
		return "", false
	}
	return word, true
}

// IsReservedCommandWord reports whether word belongs to a built-in command or another
// kind of "/" word, so no command file can have it.
func IsReservedCommandWord(word string) bool {
	first, _, nested := strings.Cut(word, ":")
	return containsWord(reservedCommandWords, word) || (nested && containsWord(reservedCommandPrefixes, first))
}

func containsWord(words []string, word string) bool {
	for _, candidate := range words {
		if candidate == word {
			return true
		}
	}
	return false
}

func takesArguments(text string) bool {
	return commandPlaceholder.MatchString(text)
}

// ExpandCommand fills a command's text with the arguments typed after its word.
func ExpandCommand(text, arguments string) string {
	arguments = strings.TrimSpace(arguments)
	if !takesArguments(text) {
		if arguments == "" {
			return text
		}
		return strings.TrimRight(text, "\n") + "\n\n" + arguments
	}
	words := commandArgumentWords(arguments)
	return commandPlaceholder.ReplaceAllStringFunc(text, func(match string) string {
		if match == "$ARGUMENTS" {
			return arguments
		}
		index := int(match[1] - '1')
		if index < len(words) {
			return words[index]
		}
		return ""
	})
}

// commandArgumentWords splits the arguments at white space; a double-quoted run is one
// word without its quotes, with \" and \\ inside it.
func commandArgumentWords(arguments string) []string {
	var words []string
	var word strings.Builder
	inWord, quoted := false, false
	for index := 0; index < len(arguments); index++ {
		character := arguments[index]
		switch {
		case quoted && character == '\\' && index+1 < len(arguments) && (arguments[index+1] == '"' || arguments[index+1] == '\\'):
			index++
			word.WriteByte(arguments[index])
		case character == '"':
			quoted, inWord = !quoted, true
		case !quoted && (character == ' ' || character == '\t' || character == '\n' || character == '\r'):
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(character)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// listCommands lists the commands of the folders in order. A word found in an earlier
// folder hides the same word in a later one; the list is sorted by word.
func listCommands(ctx context.Context, directories []string, maxBytes int64) ([]Summary, error) {
	items := []Summary{}
	seen := map[string]bool{}
	for _, root := range directories {
		info, err := os.Stat(root)
		if err != nil {
			if isSkippableFilesystemError(err) {
				continue
			}
			return nil, err
		}
		if !info.IsDir() {
			return nil, configError("a library directory for %s is not a directory", Command)
		}
		paths, err := markdownFiles(ctx, root)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			asset, err := readAsset(Command, root, path, maxBytes)
			if err != nil {
				return nil, err
			}
			if asset == nil || seen[asset.Command] {
				continue
			}
			seen[asset.Command] = true
			items = append(items, Summary{
				ID: asset.ID, Kind: Command, Name: asset.Name, Description: asset.Description,
				Source: asset.Source, CommandFields: asset.CommandFields,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Command < items[j].Command })
	return items, nil
}
