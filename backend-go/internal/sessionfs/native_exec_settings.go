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
	// defaultNativeExecInputMax bounds the JSON input written to a program's stdin.
	defaultNativeExecInputMax int64 = 1024 * 1024
	// defaultNativeExecArgMax keeps code passed as one argument well under the kernel limit
	// of a single argument (128 KiB on Linux).
	defaultNativeExecArgMax int64 = 100000
	// defaultNativeExecFunctionsDir is its own folder, never tool_smith's.
	defaultNativeExecFunctionsDir = ".jad/exec"

	// NativeExecModeArg, NativeExecModeFile and NativeExecModeStdin are the ways a program
	// receives its code: as one argument ({code}), as a temporary file ({file}), or on
	// standard input (only for a call without input).
	NativeExecModeArg   = "arg"
	NativeExecModeFile  = "file"
	NativeExecModeStdin = "stdin"

	// The built-in templates a runner may name; each installs the global fn before the code runs.
	NativeExecTemplateJS     = "js"
	NativeExecTemplatePython = "python"
	NativeExecTemplateRuby   = "ruby"

	nativeExecFilePlaceholder     = "{file}"
	nativeExecCodePlaceholder     = "{code}"
	nativeExecTemplatePlaceholder = "{template}"
	maxNativeExecArgv             = 32
)

var (
	nativeExecOwnKeys = []string{
		"arg_max_bytes", "code_max_bytes", "default_lang", "function_max_bytes", "functions_dir", "input_max_bytes",
		"line_aware", "list_header_bytes", "list_max_files", "list_max_results", "result_max_bytes", "runners",
	}
	nativeExecKeys     = append(append([]string{}, nativeBashStyleKeys...), nativeExecOwnKeys...)
	nativeExecLangKeys = []string{"aliases", "argv", "ext", "lib_env", "mode", "template"}

	nativeExecNamePattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_+.-]{0,31}$`)
	nativeExecExtensionPattern = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)
	nativeExecLibEnvPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// NativeExecLang is one interpreter exec can run.
type NativeExecLang struct {
	// Argv is the interpreter and its fixed flags; {code} is replaced by the code (mode arg),
	// {file} by the code file (mode file) and {template} by the template file (see Template).
	Argv []string
	// Mode is NativeExecModeArg, NativeExecModeFile or NativeExecModeStdin.
	Mode string
	// Ext is the extension of the temporary code file, with its dot.
	Ext string
	// Aliases are other names the model may use for this language.
	Aliases []string
	// LibEnv names the list variable (PATH, PYTHONPATH, ...) that receives the language's helper
	// directory, functions_dir/<name>, so code finds its helpers by the interpreter's own means.
	// Empty: the directory is not exposed.
	LibEnv string
	// Template names the built-in preload that installs fn (NativeExecTemplate*), replaceable by
	// functions_dir/<name>/_template<ext>. js and ruby are attached where argv has {template};
	// python is a sitecustomize.py in a directory put on PYTHONPATH. Empty: no fn.
	Template string
}

// NativeExecSettings holds the normalized settings of native.exec. Child is how its
// process runs (environment, spill, stream budgets, timeouts, path policy), read from
// tools.native.exec with the same keys and defaults as native.bash; exec never reads bash.
type NativeExecSettings struct {
	Child NativeBashSettings
	// LangNames lists Langs in the order they are shown to the model.
	LangNames []string
	Langs     map[string]NativeExecLang
	// DefaultLang is the name (never an alias) of the Langs entry a call without `lang` runs.
	DefaultLang string
	// CodeMaxBytes is the largest code text one call may carry; more is refused, never cut.
	CodeMaxBytes int64
	// ResultMaxBytes is the largest answer read back from $JAD_RESULT.
	ResultMaxBytes int64
	// InputMaxBytes is the largest JSON input one call may write to the program's stdin.
	InputMaxBytes int64
	// ArgMaxBytes is the largest code a mode arg runner receives as one argument.
	ArgMaxBytes int64
	// FunctionsDir is the saved-function folder as configured, relative to the workspace.
	FunctionsDir string
	// ListMaxResults, ListMaxFiles, ListHeaderBytes and FunctionMaxBytes bound the listing
	// and the reading of saved functions, with tool_smith's meaning.
	ListMaxResults   int64
	ListMaxFiles     int64
	ListHeaderBytes  int64
	FunctionMaxBytes int64
	// LineAware cuts a truncated stream at line boundaries.
	LineAware bool
}

// defaultNativeExecLangs are the interpreters available without configuration. The code
// is one argument; bash and sh get a fixed $0 so the call's args start at $1.
func defaultNativeExecLangs() ([]string, map[string]NativeExecLang) {
	names := []string{"python", "node", "bash", "sh", "ruby", "perl", "ts"}
	return names, map[string]NativeExecLang{
		"python": {Argv: []string{"python3", "-c", "{code}"}, Mode: NativeExecModeArg, Ext: ".py", Aliases: []string{"python3", "py"}, LibEnv: "PYTHONPATH", Template: NativeExecTemplatePython},
		"node":   {Argv: []string{"node", "-r", "{template}", "-e", "{code}"}, Mode: NativeExecModeArg, Ext: ".js", Aliases: []string{"nodejs", "js", "javascript"}, LibEnv: "NODE_PATH", Template: NativeExecTemplateJS},
		"bash":   {Argv: []string{"bash", "-c", "{code}", "exec"}, Mode: NativeExecModeArg, Ext: ".sh", Aliases: []string{"shell"}, LibEnv: "PATH"},
		"sh":     {Argv: []string{"sh", "-c", "{code}", "exec"}, Mode: NativeExecModeArg, Ext: ".sh", Aliases: []string{}, LibEnv: "PATH"},
		"ruby":   {Argv: []string{"ruby", "-r", "{template}", "-e", "{code}"}, Mode: NativeExecModeArg, Ext: ".rb", Aliases: []string{"rb"}, LibEnv: "RUBYLIB", Template: NativeExecTemplateRuby},
		"perl":   {Argv: []string{"perl", "-e", "{code}"}, Mode: NativeExecModeArg, Ext: ".pl", Aliases: []string{}, LibEnv: "PERL5LIB"},
		"ts":     {Argv: []string{"bun", "--no-install", "--preload", "{template}", "-e", "{code}"}, Mode: NativeExecModeArg, Ext: ".ts", Aliases: []string{"typescript", "bun"}, LibEnv: "NODE_PATH", Template: NativeExecTemplateJS},
	}
}

func normalizeNativeExecSettings(
	setting map[string]json.RawMessage,
	globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir, envFileBase string,
	readPolicy, grepPolicy NativePathPolicy,
) (*NativeExecSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil {
		return nil, err
	}
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
	if err := rejectRemovedApproval(fields, section, settingPath); err != nil {
		return nil, err
	}
	if _, old := fields["langs"]; old {
		return nil, configError("%s: %s.langs was renamed runners; its entries use mode (arg, file or stdin) instead of code", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeExecKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeExecKeys, "")
		}
	}

	child, err := normalizeBashStyleFields(fields, section, globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir, envFileBase, readPolicy, grepPolicy)
	if err != nil {
		return nil, err
	}
	settings := &NativeExecSettings{Child: *child}
	if settings.LineAware, err = bashBool(fields, "line_aware", defaultNativeExecLineAware, settingPath+": "+section); err != nil {
		return nil, err
	}
	if settings.LangNames, settings.Langs, err = normalizeNativeExecLangs(fields["runners"], settingPath+": "+section+".runners"); err != nil {
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
	if settings.InputMaxBytes, err = configuredInteger(fields, "input_max_bytes", defaultNativeExecInputMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ArgMaxBytes, err = configuredInteger(fields, "arg_max_bytes", defaultNativeExecArgMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.FunctionsDir, err = bashString(fields, "functions_dir", defaultNativeExecFunctionsDir, settingPath+": "+section, false); err != nil {
		return nil, err
	}
	if settings.ListMaxResults, err = configuredInteger(fields, "list_max_results", defaultNativeToolSmithListMaxResults, 1, maxNativeToolSmithListMaxResults, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ListMaxFiles, err = configuredInteger(fields, "list_max_files", defaultNativeToolSmithListMaxFiles, 1, maxNativeToolSmithListMaxFiles, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ListHeaderBytes, err = configuredInteger(fields, "list_header_bytes", defaultNativeToolSmithListHeaderBytes, 1, maxNativeToolSmithListHeaderBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.FunctionMaxBytes, err = configuredInteger(fields, "function_max_bytes", defaultNativeToolSmithFunctionMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
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
	return "", configError("%s \"%s\" is not in tools.native.exec.runners (available: %s)", path, wanted, strings.Join(names, ", "))
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
	lang := NativeExecLang{Argv: argv, Mode: NativeExecModeArg, Aliases: []string{}}
	if _, old := fields["code"]; old {
		return NativeExecLang{}, configError("%s.code was renamed mode (arg, file or stdin)", path)
	}
	if value, present := nullableField(fields, "mode"); present {
		if json.Unmarshal(value, &lang.Mode) != nil || (lang.Mode != NativeExecModeArg && lang.Mode != NativeExecModeFile && lang.Mode != NativeExecModeStdin) {
			return NativeExecLang{}, configError("%s.mode must be \"arg\", \"file\" or \"stdin\"", path)
		}
	}
	files, codes := 0, 0
	for _, item := range argv {
		if strings.Contains(item, nativeExecFilePlaceholder) {
			files++
		}
		if strings.Contains(item, nativeExecCodePlaceholder) {
			codes++
		}
	}
	wantFiles, wantCodes := 0, 0
	switch lang.Mode {
	case NativeExecModeArg:
		wantCodes = 1
	case NativeExecModeFile:
		wantFiles = 1
	}
	if (files > 0) != (wantFiles > 0) || (codes > 0) != (wantCodes > 0) {
		return NativeExecLang{}, configError("%s.argv must contain %s when mode is \"arg\", %s when mode is \"file\", and neither when mode is \"stdin\"", path, nativeExecCodePlaceholder, nativeExecFilePlaceholder)
	}
	if value, present := nullableField(fields, "ext"); present {
		if json.Unmarshal(value, &lang.Ext) != nil || (lang.Ext != "" && !nativeExecExtensionPattern.MatchString(lang.Ext)) {
			return NativeExecLang{}, configError("%s.ext must be empty or a dot and 1 to 8 letters or digits", path)
		}
	}
	if value, present := nullableField(fields, "lib_env"); present {
		if json.Unmarshal(value, &lang.LibEnv) != nil || (lang.LibEnv != "" && !nativeExecLibEnvPattern.MatchString(lang.LibEnv)) {
			return NativeExecLang{}, configError("%s.lib_env must be empty or an environment variable name", path)
		}
	}
	if value, present := nullableField(fields, "template"); present {
		if json.Unmarshal(value, &lang.Template) != nil || (lang.Template != NativeExecTemplateJS && lang.Template != NativeExecTemplatePython && lang.Template != NativeExecTemplateRuby) {
			return NativeExecLang{}, configError("%s.template must be \"js\", \"python\" or \"ruby\"", path)
		}
	}
	placeholders := 0
	for _, item := range argv {
		placeholders += strings.Count(item, nativeExecTemplatePlaceholder)
	}
	switch {
	case lang.Template == NativeExecTemplatePython && placeholders > 0:
		return NativeExecLang{}, configError("%s.argv must not contain %s: a python template is found through PYTHONPATH", path, nativeExecTemplatePlaceholder)
	case (lang.Template == NativeExecTemplateJS || lang.Template == NativeExecTemplateRuby) && placeholders != 1:
		return NativeExecLang{}, configError("%s.argv must contain %s exactly once when template is \"%s\"", path, nativeExecTemplatePlaceholder, lang.Template)
	case lang.Template == "" && placeholders > 0:
		return NativeExecLang{}, configError("%s.argv contains %s but the runner has no template", path, nativeExecTemplatePlaceholder)
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
