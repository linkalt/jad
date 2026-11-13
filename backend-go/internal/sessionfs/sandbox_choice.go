package sessionfs

import (
	"encoding/json"
	"fmt"
	"strings"

	"jad/backend-go/internal/catalog"
)

// SandboxChoice is one sandbox template a subagent or profile_message call may pick with its
// sandbox argument (tools.native.subagent.sandbox_choice,
// tools.native.profile_message.sandbox_choice; docs/isolation-sandbox-design.md §15). The
// layout loader resolves each listed name in the session's template folder.
type SandboxChoice struct {
	Name string
	// Description is the template's description, on one line, for the tool's description.
	Description string
	// Sandbox is the template's sandbox, with the session's configuration paths read-only
	// (ReadOnlyPaths); nil when the template sets enabled false, which confines nothing more
	// than the run already is.
	Sandbox *SandboxSettings
	// Isolation is the template's sandbox.isolation, read whether or not it is enabled.
	Isolation SandboxIsolation
}

// SandboxChoiceNames are the names of choices, in the order the setting lists them.
func SandboxChoiceNames(choices []SandboxChoice) []string {
	names := make([]string, len(choices))
	for index, choice := range choices {
		names[index] = choice.Name
	}
	return names
}

// FindSandboxChoice is the choice named name.
func FindSandboxChoice(choices []SandboxChoice, name string) (SandboxChoice, bool) {
	for _, choice := range choices {
		if choice.Name == name {
			return choice, true
		}
	}
	return SandboxChoice{}, false
}

// normalizeSandboxChoiceNames reads a sandbox_choice key: absent or null is off (nil); any
// other value must be a non-empty array of distinct template names. The names are resolved
// later, by the layout loader, which knows the session's template folder.
func normalizeSandboxChoiceNames(raw json.RawMessage, settingPath, section string) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var names []string
	if json.Unmarshal(raw, &names) != nil || names == nil {
		return nil, configError("%s: %s.sandbox_choice must be an array of sandbox template names", settingPath, section)
	}
	if len(names) == 0 {
		return nil, configError("%s: %s.sandbox_choice is empty; list the sandbox templates a call may pick, or leave the key out", settingPath, section)
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if strings.ContainsAny(name, `/\`) {
			return nil, configError("%s: %s.sandbox_choice entry %q: templates are flat files <folder>/<name>.json; a sub-folder is not allowed", settingPath, section, name)
		}
		if err := catalog.ValidateIdentifier(name); err != nil {
			return nil, configError("%s: %s.sandbox_choice entry %v", settingPath, section, err)
		}
		if _, dup := seen[name]; dup {
			return nil, configError("%s: %s.sandbox_choice lists %q more than once", settingPath, section, name)
		}
		seen[name] = struct{}{}
	}
	return names, nil
}

// resolveSandboxChoices reads the templates names lists from the session's template folder;
// readOnly are the session's configuration paths, kept read-only in each chosen sandbox as in
// the session's own (finishSandbox).
func (c *layoutCompose) resolveSandboxChoices(names []string, readOnly []string, where string) ([]SandboxChoice, error) {
	if names == nil {
		return nil, nil
	}
	choices := make([]SandboxChoice, 0, len(names))
	for _, name := range names {
		fields, description, err := c.templates.readDescribed(name, where)
		if err != nil {
			return nil, err
		}
		fields["template"] = mustJSON(name)
		own, enabled, err := parseSandbox(mustJSON(fields), where, "sandbox")
		if err != nil {
			return nil, err
		}
		choice := SandboxChoice{Name: name, Description: oneLineText(description), Isolation: own.Isolation}
		if enabled {
			own.ReadOnlyPaths = readOnly
			c.addFixedHidePaths(own)
			choice.Sandbox = own
		}
		choices = append(choices, choice)
	}
	return choices, nil
}

// finishSandboxChoices resolves both tools' sandbox_choice lists.
func (c *layoutCompose) finishSandboxChoices(config *SessionConfig, readOnly []string) error {
	var err error
	if settings := config.NativeSubagent; settings != nil {
		where := fmt.Sprintf("%s: tools.native.subagent.sandbox_choice", c.sessionPath)
		if settings.SandboxChoices, err = c.resolveSandboxChoices(settings.SandboxChoice, readOnly, where); err != nil {
			return err
		}
	}
	if settings := config.NativeProfileMessage; settings != nil {
		where := fmt.Sprintf("%s: tools.native.profile_message.sandbox_choice", c.sessionPath)
		if settings.SandboxChoices, err = c.resolveSandboxChoices(settings.SandboxChoice, readOnly, where); err != nil {
			return err
		}
	}
	return nil
}

// oneLineText is text with its runs of white space, line breaks included, made one space.
func oneLineText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
