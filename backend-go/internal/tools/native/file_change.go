package native

import "jad/backend-go/internal/tools"

// reportFileChange tells the call's OnFileChange hook, if any, that a file was changed.
func reportFileChange(execution tools.ExecutionContext, path, action string) {
	if execution.OnFileChange != nil {
		execution.OnFileChange(path, action)
	}
}
