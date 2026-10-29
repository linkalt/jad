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

// execTemplateAttachment is how one call attaches its runner's template: the directory to put
// in front of PYTHONPATH (python only). The argv is substituted by the plan of the call, which
// knows execTemplatePath before the code goes in, so the code is never rewritten (P6BM-02).
type execTemplateAttachment struct {
	pythonPath string
}

// execTemplatePath is where attachExecTemplate writes the template of the runner, and so what a
// {template} in the configured argv becomes. Python takes a directory instead (PYTHONPATH), and
// its argv must not carry {template}.
func execTemplatePath(lang sessionfs.NativeExecLang, dir string) string {
	ext := lang.Ext
	if ext == "" {
		ext = map[string]string{sessionfs.NativeExecTemplateJS: ".js", sessionfs.NativeExecTemplateRuby: ".rb"}[lang.Template]
	}
	return filepath.Join(dir, "template"+ext)
}

// attachExecTemplate writes the runner's template into dir, which is removed with the call, and
// returns the attachment. The text is the file functions_dir/<runner>/_template<ext> when the
// library has one (a regular file inside the library), else the built-in.
func attachExecTemplate(settings sessionfs.NativeExecSettings, lang sessionfs.NativeExecLang, runner, workspaceRoot, dir string) (execTemplateAttachment, error) {
	var attachment execTemplateAttachment
	if lang.Template == "" {
		return attachment, nil
	}
	text, err := execTemplateText(settings, lang, runner, workspaceRoot)
	if err != nil {
		return attachment, err
	}
	if lang.Template == sessionfs.NativeExecTemplatePython {
		tplDir := filepath.Join(dir, "template")
		if err := os.MkdirAll(tplDir, 0o700); err != nil {
			return attachment, err
		}
		attachment.pythonPath = tplDir
		return attachment, os.WriteFile(filepath.Join(tplDir, "sitecustomize.py"), text, 0o600)
	}
	path := execTemplatePath(lang, dir)
	if err := os.WriteFile(path, text, 0o600); err != nil {
		return attachment, err
	}
	return attachment, nil
}

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
