package sessionfs

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	// defaultNativeExecCodeMax leaves room for a long inline program; it is the same order
	// as a bash stdin.
	defaultNativeExecCodeMax int64 = 256 * 1024
	// defaultNativeExecResultMax: a result is meant to be short, the output budgets are for
	// raw output.
	defaultNativeExecResultMax int64 = 16 * 1024
	defaultNativeExecLineAware       = true
	// defaultNativeExecLang is the interpreter a call without `lang` runs.
	defaultNativeExecLang = "bash"

	// NativeExecCodeStdin and NativeExecCodeFile are the two ways a program receives its code.
	NativeExecCodeStdin = "stdin"
	NativeExecCodeFile  = "file"

	nativeExecFilePlaceholder = "{file}"
	maxNativeExecArgv         = 32
)

var (
	nativeExecKeys     = []string{"approval", "code_max_bytes", "default_lang", "langs", "line_aware", "result_max_bytes"}
	nativeExecLangKeys = []string{"aliases", "argv", "code", "ext"}

	nativeExecNamePattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_+.-]{0,31}$`)
	nativeExecExtensionPattern = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)
)

// NativeExecLang is one interpreter exec can run.
type NativeExecLang struct {
	// Argv is the interpreter and its fixed flags; {file} is replaced by the code file.
	Argv []string
	// Code is NativeExecCodeStdin or NativeExecCodeFile.
	Code string
	// Ext is the extension of the temporary code file (file delivery only), with its dot.
	Ext string
	// Aliases are other names the model may use for this language.
	Aliases []string
}

// NativeExecSettings holds the normalized settings of native.exec. How a child runs
// (environment, spill, stream budgets, timeouts, path policy) is native.bash's.
type NativeExecSettings struct {
	// Approval is "off" (default) or "ask".
	Approval string
	// LangNames lists Langs in the order they are shown to the model.
	LangNames []string
	Langs     map[string]NativeExecLang
	// DefaultLang is the name (never an alias) of the Langs entry a call without `lang` runs.
	DefaultLang string
	// CodeMaxBytes is the largest code text one call may carry; more is refused, never cut.
	CodeMaxBytes int64
	// ResultMaxBytes is the largest answer read back from $EXEC_RESULT.
	ResultMaxBytes int64
	// LineAware cuts a truncated stream at line boundaries.
	LineAware bool
}

// defaultNativeExecLangs are the interpreters available without configuration. The program
// reads its code from standard input, so the working directory behaves as in a shell.
func defaultNativeExecLangs() ([]string, map[string]NativeExecLang) {
	names := []string{"python", "node", "bash", "sh", "ruby", "perl"}
	return names, map[string]NativeExecLang{
		"python": {Argv: []string{"python3", "-"}, Code: NativeExecCodeStdin, Ext: ".py", Aliases: []string{"python3", "py"}},
		"node":   {Argv: []string{"node", "-"}, Code: NativeExecCodeStdin, Ext: ".js", Aliases: []string{"nodejs", "js", "javascript"}},
		"bash":   {Argv: []string{"bash", "-s", "--"}, Code: NativeExecCodeStdin, Ext: ".sh", Aliases: []string{"shell"}},
		"sh":     {Argv: []string{"sh", "-s", "--"}, Code: NativeExecCodeStdin, Ext: ".sh", Aliases: []string{}},
		"ruby":   {Argv: []string{"ruby", "-"}, Code: NativeExecCodeStdin, Ext: ".rb", Aliases: []string{"rb"}},
		"perl":   {Argv: []string{"perl", "-"}, Code: NativeExecCodeStdin, Ext: ".pl", Aliases: []string{}},
	}
}

func normalizeNativeExecSettings(setting map[string]json.RawMessage, settingPath string, bash *NativeBashSettings) (*NativeExecSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := toolFields["native"]
	if !exists || strings.TrimSpace(string(nativeRaw)) == "null" {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["exec"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.exec"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeExecKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeExecKeys, "")
		}
	}
	if bash == nil {
		// The child's environment, spill files, stream budgets, timeouts and path policy are
		// bash's; running without them would mean a second copy of every default.
		return nil, configError("%s: tools.native.exec needs tools.native.bash, whose child-process settings it uses", settingPath)
	}

	settings := &NativeExecSettings{}
	if settings.LineAware, err = bashBool(fields, "line_aware", defaultNativeExecLineAware, settingPath+": "+section); err != nil {
		return nil, err
	}
	if settings.Approval, err = normalizeApprovalMode(fields, section, settingPath); err != nil {
		return nil, err
	}
	if settings.LangNames, settings.Langs, err = normalizeNativeExecLangs(fields["langs"], settingPath+": "+section+".langs"); err != nil {
		return nil, err
	}
	if settings.DefaultLang, err = normalizeNativeExecDefaultLang(fields, settings.LangNames, settings.Langs, settingPath+": "+section+".default_lang"); err != nil {
		return nil, err
	}
	if settings.CodeMaxBytes, err = configuredInteger(fields, "code_max_bytes", defaultNativeExecCodeMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ResultMaxBytes, err = configuredInteger(fields, "result_max_bytes", defaultNativeExecResultMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	return settings, nil
}

// normalizeNativeExecLangs merges the configured entries over the built-in ones by name.
// A name or alias that two entries both claim is refused, because the model could not tell
// which program it would get.
func normalizeNativeExecLangs(raw json.RawMessage, path string) ([]string, map[string]NativeExecLang, error) {
	names, langs := defaultNativeExecLangs()
	if len(raw) > 0 {
		entries, err := decodeConfigObject(raw, path)
		if err != nil {
			return nil, nil, configError("%s must be an object", path)
		}
		order, err := orderedObjectKeys(raw)
		if err != nil {
			return nil, nil, configError("%s must be an object", path)
		}
		for _, name := range order {
			entryPath := path + "." + name
			if !nativeExecNamePattern.MatchString(name) {
				return nil, nil, configError("%s is not a valid language name (lowercase letters, digits, _ + . -)", entryPath)
			}
			lang, err := normalizeNativeExecLang(entries[name], entryPath)
			if err != nil {
				return nil, nil, err
			}
			if _, replaced := langs[name]; !replaced {
				names = append(names, name)
			}
			langs[name] = lang
		}
	}
	claimed := map[string]string{}
	for _, name := range names {
		for _, label := range append([]string{name}, langs[name].Aliases...) {
			if owner, taken := claimed[label]; taken && owner != name {
				return nil, nil, configError("%s: \"%s\" names both %s and %s", path, label, owner, name)
			}
			claimed[label] = name
		}
	}
	return names, langs, nil
}

// normalizeNativeExecDefaultLang resolves the language a call without `lang` runs to its
// table name, so a later lookup cannot miss. An alias is accepted; a name the table does
// not know is refused at load time instead of failing every call.
func normalizeNativeExecDefaultLang(fields map[string]json.RawMessage, names []string, langs map[string]NativeExecLang, path string) (string, error) {
	wanted := defaultNativeExecLang
	if raw, present := nullableField(fields, "default_lang"); present {
		if json.Unmarshal(raw, &wanted) != nil {
			return "", configError("%s must be a language name", path)
		}
	}
	lowered := strings.ToLower(strings.TrimSpace(wanted))
	for _, name := range names {
		if name == lowered || containsString(langs[name].Aliases, lowered) {
			return name, nil
		}
	}
	return "", configError("%s \"%s\" is not in tools.native.exec.langs (available: %s)", path, wanted, strings.Join(names, ", "))
}

func normalizeNativeExecLang(raw json.RawMessage, path string) (NativeExecLang, error) {
	fields, err := decodeConfigObject(raw, path)
	if err != nil {
		return NativeExecLang{}, configError("%s must be an object", path)
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return NativeExecLang{}, configError("%s must be an object", path)
	}
	for _, key := range keys {
		if !containsString(nativeExecLangKeys, key) {
			return NativeExecLang{}, unsupportedToolKey(path, key, nativeExecLangKeys, "")
		}
	}
	var argv []string
	argvError := configError("%s.argv must be 1 to 32 non-empty strings: the interpreter and its flags", path)
	var rawArgv []json.RawMessage
	if json.Unmarshal(fields["argv"], &rawArgv) != nil || len(rawArgv) == 0 || len(rawArgv) > maxNativeExecArgv {
		return NativeExecLang{}, argvError
	}
	for _, item := range rawArgv {
		var text string
		if json.Unmarshal(item, &text) != nil || text == "" {
			return NativeExecLang{}, argvError
		}
		argv = append(argv, text)
	}
	lang := NativeExecLang{Argv: argv, Code: NativeExecCodeStdin, Aliases: []string{}}
	if value, present := nullableField(fields, "code"); present {
		if json.Unmarshal(value, &lang.Code) != nil || (lang.Code != NativeExecCodeStdin && lang.Code != NativeExecCodeFile) {
			return NativeExecLang{}, configError("%s.code must be \"stdin\" or \"file\"", path)
		}
	}
	placeholders := 0
	for _, item := range argv {
		if strings.Contains(item, nativeExecFilePlaceholder) {
			placeholders++
		}
	}
	if lang.Code == NativeExecCodeFile && placeholders == 0 {
		return NativeExecLang{}, configError("%s.argv must contain %s when code is \"file\"", path, nativeExecFilePlaceholder)
	}
	if lang.Code == NativeExecCodeStdin && placeholders > 0 {
		return NativeExecLang{}, configError("%s.argv must not contain %s when code is \"stdin\"", path, nativeExecFilePlaceholder)
	}
	if value, present := nullableField(fields, "ext"); present {
		if json.Unmarshal(value, &lang.Ext) != nil || (lang.Ext != "" && !nativeExecExtensionPattern.MatchString(lang.Ext)) {
			return NativeExecLang{}, configError("%s.ext must be empty or a dot and 1 to 8 letters or digits", path)
		}
	}
	if value, present := nullableField(fields, "aliases"); present {
		var rawAliases []json.RawMessage
		if json.Unmarshal(value, &rawAliases) != nil {
			return NativeExecLang{}, configError("%s.aliases must be language names", path)
		}
		for _, item := range rawAliases {
			var alias string
			if json.Unmarshal(item, &alias) != nil || !nativeExecNamePattern.MatchString(alias) {
				return NativeExecLang{}, configError("%s.aliases must be language names", path)
			}
			lang.Aliases = append(lang.Aliases, alias)
		}
	}
	return lang, nil
}

// nullableField returns a field's raw value, treating an explicit null like an absent key
// (the TypeScript loader reads these with `??`).
func nullableField(fields map[string]json.RawMessage, name string) (json.RawMessage, bool) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return nil, false
	}
	return raw, true
}
