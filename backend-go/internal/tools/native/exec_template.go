package native

import (
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"jad/backend-go/internal/sessionfs"
)

// execTemplateFilePrefix marks a file in a runner folder that replaces the built-in template; it is not a helper.
const execTemplateFilePrefix = "_template"

//go:embed exec_templates/js.js exec_templates/python.py exec_templates/ruby.rb
var execTemplateFiles embed.FS

var execTemplateSources = map[string]string{
	sessionfs.NativeExecTemplateJS:     "exec_templates/js.js",
	sessionfs.NativeExecTemplatePython: "exec_templates/python.py",
	sessionfs.NativeExecTemplateRuby:   "exec_templates/ruby.rb",
}

// execTemplatePath is where a call writes the template of the runner (execPlan.launch), and so
// what a {template} in the configured argv becomes. Python takes a directory instead (PYTHONPATH), and
// its argv must not carry {template}.
func execTemplatePath(lang sessionfs.NativeExecLang, dir string) string {
	ext := lang.Ext
	if ext == "" {
		ext = map[string]string{sessionfs.NativeExecTemplateJS: ".js", sessionfs.NativeExecTemplateRuby: ".rb"}[lang.Template]
	}
	return filepath.Join(dir, "template"+ext)
}

// execTemplateText is the runner's template: the file functions_dir/<runner>/_template<ext>
// when the library has one (a regular file inside the library), else the built-in.
func execTemplateText(settings sessionfs.NativeExecSettings, lang sessionfs.NativeExecLang, runner, workspaceRoot string) ([]byte, error) {
	if root, realRoot, err := resolveToolSmithFunctionsDir(settings.FunctionsDir, workspaceRoot); err == nil {
		ext := lang.Ext
		if ext == "" {
			ext = ".txt"
		}
		file := runner + "/" + execTemplateFilePrefix + ext
		if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(file))); statErr == nil {
			handle, problem := openExecFunction(root, realRoot, file, settings.FunctionMaxBytes)
			if problem != "" {
				return nil, fmt.Errorf("%s: %s", file, problem)
			}
			defer handle.Close()
			return io.ReadAll(handle)
		}
	}
	return execTemplateFiles.ReadFile(execTemplateSources[lang.Template])
}
