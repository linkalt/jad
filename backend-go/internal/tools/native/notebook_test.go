package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var sampleNotebookManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "notebook.json")

// jupyterNotebook is laid out as Jupyter writes it: keys sorted, one space of indent,
// sources as lists of lines, non-ASCII as is, a final newline.
const jupyterNotebook = `{
 "cells": [
  {
   "cell_type": "markdown",
   "id": "intro",
   "metadata": {},
   "source": [
    "# Résumé <of> data\n",
    "Second line"
   ]
  },
  {
   "cell_type": "code",
   "execution_count": 3,
   "id": "load",
   "metadata": {
    "tags": [
     "setup"
    ]
   },
   "outputs": [
    {
     "name": "stdout",
     "output_type": "stream",
     "text": [
      "loaded 1200 rows\n"
     ]
    },
    {
     "data": {
      "image/png": "iVBORw0KGgo=",
      "text/plain": [
       "<Figure size 640x480>"
      ]
     },
     "metadata": {},
     "output_type": "display_data"
    },
    {
     "ename": "NameError",
     "evalue": "name 'x' is not defined",
     "output_type": "error",
     "traceback": [
      "\u001b[0;31mNameError\u001b[0m: name 'x' is not defined"
     ]
    }
   ],
   "source": [
    "import pandas as pd\n",
    "df = pd.read_csv('data.csv')"
   ]
  }
 ],
 "metadata": {
  "kernelspec": {
   "display_name": "Python 3",
   "language": "python",
   "name": "python3"
  },
  "language_info": {
   "name": "python",
   "version": "3.12.1"
  }
 },
 "nbformat": 4,
 "nbformat_minor": 5
}
`

func notebookTool(t *testing.T, root string, maxOutputChars int64) tools.Tool {
	t.Helper()
	tool, err := NewNotebookTool(sessionfs.NativeNotebookSettings{Edit: testEditSettings(root, 1<<20), MaxOutputChars: maxOutputChars}, NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func writeNotebook(t *testing.T, root, text string) string {
	t.Helper()
	path := filepath.Join(root, "analysis.ipynb")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func callNotebook(t *testing.T, tool tools.Tool, root string, arguments map[string]any) string {
	t.Helper()
	output, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "call-1", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func TestNotebookReadShowsCellsAndOutputs(t *testing.T) {
	root := t.TempDir()
	writeNotebook(t, root, jupyterNotebook)
	output := callNotebook(t, notebookTool(t, root, 10), root, map[string]any{"action": "read", "path": "analysis.ipynb"})
	for _, want := range []string{
		`"summary":"Read 2 of 2 cells of analysis.ipynb"`, "cells: 2", "shown: 0-1", "language: python",
		"### cell 0 [markdown] id=intro\n# Résumé <of> data\nSecond line\n",
		"### cell 1 [code] id=load execution_count=3\nimport pandas as pd\ndf = pd.read_csv('data.csv')\n",
		"--- output 1: stream stdout ---\nloaded 120\n[... 7 more characters]",
		"--- output 2: display_data ---\n<Figure si\n[... 11 more characters]\n[image/png, 12 characters, not shown]",
		"--- output 3: error NameError: name 'x' is not defined ---\nNameError:",
		"### cell 1",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in:\n%s", want, output)
		}
	}
	if strings.Contains(output, "\x1b") {
		t.Error("the traceback kept its ANSI colours")
	}
	output = callNotebook(t, notebookTool(t, root, 0), root, map[string]any{"action": "read", "path": "analysis.ipynb", "cell": 1, "count": 5})
	if !strings.Contains(output, "shown: 1-1") || strings.Contains(output, "cell 0") || !strings.Contains(output, "[17 characters, not shown: max_output_chars is 0]") {
		t.Fatalf("read from cell 1 with no output text:\n%s", output)
	}
}

// An edit rewrites only the changed cell: the rest of the file is byte for byte as Jupyter
// wrote it.
func TestNotebookEditKeepsJupyterLayout(t *testing.T) {
	root := t.TempDir()
	path := writeNotebook(t, root, jupyterNotebook)
	output := callNotebook(t, notebookTool(t, root, 2000), root, map[string]any{"action": "edit", "path": "analysis.ipynb", "cell_id": "load", "source": "import pandas as pd\ndf = pd.read_csv('other.csv')\n"})
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "outputs_cleared: true") || !strings.Contains(output, "cell: 1") {
		t.Fatalf("edit:\n%s", output)
	}
	data, _ := os.ReadFile(path)
	start := strings.Index(jupyterNotebook, `   "execution_count": 3,`)
	end := strings.Index(jupyterNotebook, "  }\n ],\n \"metadata\"")
	want := jupyterNotebook[:start] + `   "execution_count": null,
   "id": "load",
   "metadata": {
    "tags": [
     "setup"
    ]
   },
   "outputs": [],
   "source": [
    "import pandas as pd\n",
    "df = pd.read_csv('other.csv')\n"
   ]
` + jupyterNotebook[end:]
	if string(data) != want {
		t.Fatalf("file after edit:\n%s\nwant:\n%s", data, want)
	}
}

func TestNotebookInsertDeleteAndTypeChange(t *testing.T) {
	root := t.TempDir()
	path := writeNotebook(t, root, jupyterNotebook)
	tool := notebookTool(t, root, 2000)
	output := callNotebook(t, tool, root, map[string]any{"action": "insert", "path": "analysis.ipynb", "cell": 1, "cell_type": "markdown", "source": "## Load"})
	if !strings.Contains(output, "Inserted cell 1 of analysis.ipynb") || !strings.Contains(output, "cell_id: ") || !strings.Contains(output, "cells: 3") {
		t.Fatalf("insert:\n%s", output)
	}
	callNotebook(t, tool, root, map[string]any{"action": "insert", "path": "analysis.ipynb", "source": "print(1)"})
	callNotebook(t, tool, root, map[string]any{"action": "edit", "path": "analysis.ipynb", "cell": 0, "cell_type": "raw", "source": "raw text"})
	output = callNotebook(t, tool, root, map[string]any{"action": "edit", "path": "analysis.ipynb", "cell": 1, "cell_type": "code", "source": "x = 1"})
	if !strings.Contains(output, "cell_type: code") {
		t.Fatalf("type change:\n%s", output)
	}
	callNotebook(t, tool, root, map[string]any{"action": "delete", "path": "analysis.ipynb", "cell_id": "load"})
	var document struct {
		Cells []map[string]any `json:"cells"`
	}
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	var shape []string
	for _, cell := range document.Cells {
		_, hasOutputs := cell["outputs"]
		_, hasCount := cell["execution_count"]
		_, hasID := cell["id"]
		shape = append(shape, cell["cell_type"].(string)+":"+notebookSource(cell["source"])+":"+map[bool]string{true: "outputs", false: "-"}[hasOutputs && hasCount]+":"+map[bool]string{true: "id", false: "-"}[hasID])
	}
	want := []string{"raw:raw text:-:id", "code:x = 1:outputs:id", "code:print(1):outputs:id"}
	if !reflect.DeepEqual(shape, want) {
		t.Fatalf("cells = %v, want %v", shape, want)
	}
}

// Cells get ids only from nbformat 4.5 on, as Jupyter does.
func TestNotebookInsertWithoutIDsBeforeFormat45(t *testing.T) {
	root := t.TempDir()
	path := writeNotebook(t, root, `{"cells":[],"metadata":{},"nbformat":4,"nbformat_minor":4}`)
	output := callNotebook(t, notebookTool(t, root, 2000), root, map[string]any{"action": "insert", "path": "analysis.ipynb", "source": "a\nb\n"})
	if strings.Contains(output, "cell_id") {
		t.Fatalf("insert:\n%s", output)
	}
	data, _ := os.ReadFile(path)
	want := "{\n \"cells\": [\n  {\n   \"cell_type\": \"code\",\n   \"execution_count\": null,\n   \"metadata\": {},\n   \"outputs\": [],\n   \"source\": [\n    \"a\\n\",\n    \"b\\n\"\n   ]\n  }\n ],\n \"metadata\": {},\n \"nbformat\": 4,\n \"nbformat_minor\": 4\n}\n"
	if string(data) != want {
		t.Fatalf("file:\n%s", data)
	}
}

func TestNotebookRefusals(t *testing.T) {
	root := t.TempDir()
	path := writeNotebook(t, root, jupyterNotebook)
	tool := notebookTool(t, root, 2000)
	for name, test := range map[string]struct {
		arguments map[string]any
		want      string
	}{
		"missing file":      {map[string]any{"action": "read", "path": "none.ipynb"}, "error_code: file_not_found"},
		"edit no source":    {map[string]any{"action": "edit", "path": "analysis.ipynb", "cell": 0}, "error_code: source_required"},
		"edit no cell":      {map[string]any{"action": "edit", "path": "analysis.ipynb", "source": "x"}, "error_code: cell_required"},
		"cell out of range": {map[string]any{"action": "delete", "path": "analysis.ipynb", "cell": 2}, "There is no cell 2"},
		"unknown id":        {map[string]any{"action": "delete", "path": "analysis.ipynb", "cell_id": "nope"}, "No cell has id nope"},
		"insert too far":    {map[string]any{"action": "insert", "path": "analysis.ipynb", "cell": 3, "source": "x"}, "Cannot insert before cell 3"},
		"outside workspace": {map[string]any{"action": "read", "path": filepath.Join(t.TempDir(), "x.ipynb")}, "error_code: path_outside_workspace"},
	} {
		if output := callNotebook(t, tool, root, test.arguments); !strings.Contains(output, test.want) {
			t.Errorf("%s: missing %q in:\n%s", name, test.want, output)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != jupyterNotebook {
		t.Fatal("a refused call changed the notebook")
	}
	for name, text := range map[string]string{"not json": "hello", "format 3": `{"nbformat":3,"worksheets":[]}`, "no cells": `{"nbformat":4}`, "cell not object": `{"nbformat":4,"cells":[1]}`} {
		writeNotebook(t, root, text)
		if output := callNotebook(t, tool, root, map[string]any{"action": "read", "path": "analysis.ipynb"}); !strings.Contains(output, "error_code: not_a_notebook") {
			t.Errorf("%s:\n%s", name, output)
		}
	}
}

func TestNotebookIsNotReadOnlyAndLoadsFromTheSampleManifest(t *testing.T) {
	root := t.TempDir()
	tool := notebookTool(t, root, 2000)
	if tool.ReadOnly || !tool.LeafBudget {
		t.Fatal("notebook writes: it must not be offered to read-only sub-agents")
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath: filepath.Join(root, "setting.json"), ToolDefinitionPaths: []string{sampleNotebookManifest},
		NativeNotebook: &sessionfs.NativeNotebookSettings{Edit: testEditSettings(root, 1<<20), MaxOutputChars: 2000},
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "notebook" || definitions[0].Description != notebookDescription {
		t.Fatalf("definitions = %+v", definitions)
	}
	var manifest, code any
	_ = json.Unmarshal(definitions[0].InputSchema, &manifest)
	_ = json.Unmarshal(notebookInputSchema, &code)
	if !reflect.DeepEqual(manifest, code) {
		t.Fatal("the sample manifest's input schema differs from notebookInputSchema")
	}
}

// An output text without a final newline still ends its line before the next one.
func TestNotebookReadEndsEveryOutputLine(t *testing.T) {
	root := t.TempDir()
	writeNotebook(t, root, `{"cells":[{"cell_type":"code","execution_count":1,"metadata":{},"outputs":[{"data":{"image/png":"iVBOR","text/plain":"1"},"execution_count":1,"metadata":{},"output_type":"execute_result"},{"name":"stdout","output_type":"stream","text":"no newline"}],"source":"1"}],"metadata":{},"nbformat":4,"nbformat_minor":5}`)
	output := callNotebook(t, notebookTool(t, root, 2000), root, map[string]any{"action": "read", "path": "analysis.ipynb"})
	if !strings.Contains(output, "--- output 1: execute_result ---\n1\n[image/png, 5 characters, not shown]\n--- output 2: stream stdout ---\nno newline\n\n--- END CONTENT") {
		t.Fatalf("read:\n%s", output)
	}
}
