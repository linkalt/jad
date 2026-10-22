package native

import (
	"context"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// executeNativeProcess runs one process call without an async host: nothing is tracked.
func executeNativeProcess(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext, settings sessionfs.NativeProcessSettings, manager ProcessManager, apiKeyEnv string) (string, error) {
	return executeNativeProcessAsync(ctx, arguments, execution, settings, manager, apiKeyEnv, nil)
}
