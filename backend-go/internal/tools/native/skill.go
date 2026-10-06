package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"example.com/agent-harness/backend-go/internal/contentlibrary"
	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

const skillToolID = "native.skill"

var skillInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "name":{"type":"string","description":"Skill name from the list."}
  },
  "additionalProperties":false
}`)

const skillDescription = "Load a skill's instructions by name. Call without a name to list available skills."

// skillLibrary is what the skill tool needs from the skill libraries. It is
// resolved on every call, so a change to setting.json applies without a restart.
type skillLibrary interface {
	Directories() ([]string, error)
	List(ctx context.Context, directories []string) ([]contentlibrary.Asset, error)
	Read(ctx context.Context, directories []string, name string) (contentlibrary.LoadedSkill, bool, error)
}

type rootSkillLibrary struct{ globalJadRoot string }

func (l rootSkillLibrary) Directories() ([]string, error) {
	return contentlibrary.SkillDirectories(l.globalJadRoot)
}

func (rootSkillLibrary) List(ctx context.Context, directories []string) ([]contentlibrary.Asset, error) {
	return contentlibrary.ListSkills(ctx, directories)
}

func (rootSkillLibrary) Read(ctx context.Context, directories []string, name string) (contentlibrary.LoadedSkill, bool, error) {
	return contentlibrary.ReadSkill(ctx, directories, name)
}

// NewSkillTool creates the read-only bridge from the global root's skill
// libraries to the model.
func NewSkillTool(settings sessionfs.NativeSkillSettings, globalJadRoot string) (tools.Tool, error) {
	return newSkillTool(settings, rootSkillLibrary{globalJadRoot: globalJadRoot})
}

func newSkillTool(settings sessionfs.NativeSkillSettings, library skillLibrary) (tools.Tool, error) {
	validator, err := tools.CompileSchema(skillInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native skill schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          skillToolID,
			Source:      "native",
			Name:        "skill",
			Description: skillDescription,
			InputSchema: append(json.RawMessage(nil), skillInputSchema...),
		},
		Validator:          validator,
		ReadOnly:           true,
		ExampleOperation:   json.RawMessage(`{"name":"agent-tool-design-guidelines"}`),
		NormalizeArguments: normalizeSkillArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeSkill(ctx, arguments, settings, library, execution.CallID)
		},
	}, nil
}

// normalizeSkillArguments matches skill names tolerantly: they are lowercase and
// hyphenated, and a model often writes `Agent_Tool Design` for
// `agent-tool-design`. An empty name means "list", so a blank string is dropped
// instead of failing as an unknown skill.
func normalizeSkillArguments(arguments map[string]any) map[string]any {
	name, ok := arguments["name"].(string)
	if !ok {
		return arguments
	}
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	name = hyphenateSkillName(strings.ToLower(trimJSSpace(name)))
	if name == "" {
		delete(normalized, "name")
	} else {
		normalized["name"] = name
	}
	return normalized
}

// hyphenateSkillName replaces each run of whitespace and underscores with one
// hyphen, like JavaScript's replace(/[\s_]+/g, "-").
func hyphenateSkillName(value string) string {
	var builder strings.Builder
	separator := false
	for _, character := range value {
		if character == '_' || isJSSpace(character) {
			if !separator {
				builder.WriteByte('-')
			}
			separator = true
			continue
		}
		separator = false
		builder.WriteRune(character)
	}
	return builder.String()
}

// isJSSpace reports whether the character is JavaScript whitespace, which is
// not the same set as Go's unicode.IsSpace: JavaScript includes U+FEFF and
// excludes U+0085.
func isJSSpace(character rune) bool {
	switch character {
	case '\ufeff':
		return true
	case '\u0085':
		return false
	}
	return unicode.IsSpace(character)
}

func trimJSSpace(value string) string {
	return strings.TrimFunc(value, isJSSpace)
}

// oneLine collapses every whitespace run to a single space, like
// value.replace(/\s+/g, " ").trim().
func oneLine(value string) string {
	return strings.Join(strings.FieldsFunc(value, isJSSpace), " ")
}

// cutUTF8 cuts text to at most maxBytes bytes without leaving half a character.
func cutUTF8(text string, maxBytes int64) (string, bool) {
	if int64(len(text)) <= maxBytes {
		return text, false
	}
	end := int(maxBytes)
	// Step back over continuation bytes so the cut lands on a character boundary.
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end], true
}

func executeNativeSkill(ctx context.Context, arguments map[string]any, settings sessionfs.NativeSkillSettings, library skillLibrary, callID string) (string, error) {
	directories, err := library.Directories()
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "skill_directories_unavailable", "Skill directories could not be loaded",
			tools.Information{Key: "reason", Value: err.Error()}), nil
	}
	name, hasName := arguments["name"].(string)
	if !hasName {
		skills, err := library.List(ctx, directories)
		if err != nil {
			return "", fmt.Errorf("list skills: %w", err)
		}
		return renderSkillList(callID, skills, settings.MaxBytes)
	}

	found, exists, err := library.Read(ctx, directories, name)
	if err != nil {
		return "", fmt.Errorf("read skill: %w", err)
	}
	if !exists {
		// Recoverable: hand back the valid names so the model can correct itself
		// in one step instead of probing.
		skills, err := library.List(ctx, directories)
		if err != nil {
			return "", fmt.Errorf("list skills: %w", err)
		}
		names := make([]string, 0, len(skills))
		for _, skill := range skills {
			names = append(names, skill.Name)
		}
		available := "none"
		if len(names) > 0 {
			available = strings.Join(names, ", ")
		}
		return tools.ExecutionError(callID, tools.StatusNotFound, "skill_not_found", fmt.Sprintf("Skill %s was not found", name),
			tools.Information{Key: "skill", Value: name}, tools.Information{Key: "available", Value: available}), nil
	}

	text, cut := cutUTF8(found.Asset.Content, settings.MaxBytes)
	information := []tools.Information{
		{Key: "name", Value: found.Asset.Name},
		// Relative files the skill mentions resolve against this directory.
		{Key: "directory", Value: found.Directory},
		{Key: "content_bytes", Value: len(found.Asset.Content)},
	}
	if cut {
		information = append(information, tools.Information{Key: "truncated", Value: true})
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     "Loaded skill " + found.Asset.Name,
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: text},
	})
}

func renderSkillList(callID string, skills []contentlibrary.Asset, maxBytes int64) (string, error) {
	if len(skills) == 0 {
		return tools.RenderToolResult(tools.ResultOptions{
			ID:          callID,
			Status:      tools.StatusOK,
			Summary:     "No skills are available",
			Information: []tools.Information{{Key: "skills", Value: 0}},
		})
	}
	lines := make([]string, 0, len(skills))
	for _, skill := range skills {
		lines = append(lines, skill.Name+": "+oneLine(skill.Description))
	}
	text, cut := cutUTF8(strings.Join(lines, "\n"), maxBytes)
	plural := "s"
	if len(skills) == 1 {
		plural = ""
	}
	information := []tools.Information{{Key: "skills", Value: len(skills)}}
	if cut {
		information = append(information, tools.Information{Key: "truncated", Value: true})
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     fmt.Sprintf("%d skill%s available", len(skills), plural),
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: text},
	})
}
