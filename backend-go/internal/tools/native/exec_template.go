package native

import (
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// execTemplateAttachment is how one call attaches its runner's template: argv with {template}
// replaced, and the directory to put in front of PYTHONPATH (python only).
type execTemplateAttachment struct {
	argv       []string
	pythonPath string
}

// attachExecTemplate writes the runner's template into dir, which is removed with the call, and
// returns the attachment. The text is the file functions_dir/<runner>/_template<ext> when the
// library has one (a regular file inside the library), else the built-in.
func attachExecTemplate(settings sessionfs.NativeExecSettings, lang sessionfs.NativeExecLang, runner, workspaceRoot, dir string, argv []string) (execTemplateAttachment, error) {
	attachment := execTemplateAttachment{argv: argv}
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
	ext := lang.Ext
	if ext == "" {
		ext = map[string]string{sessionfs.NativeExecTemplateJS: ".js", sessionfs.NativeExecTemplateRuby: ".rb"}[lang.Template]
	}
	path := filepath.Join(dir, "template"+ext)
	if err := os.WriteFile(path, text, 0o600); err != nil {
		return attachment, err
	}
	attachment.argv = make([]string, len(argv))
	for index, item := range argv {
		attachment.argv[index] = strings.ReplaceAll(item, "{template}", path)
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
