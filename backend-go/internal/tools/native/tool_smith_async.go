package native

import (
	"context"
	"regexp"
	"time"

	"jad/backend-go/internal/asyncmgr"
)

const asyncKindToolSmith asyncmgr.Kind = "tool_smith"

var toolSmithRunID = regexp.MustCompile(`^smith_[0-9a-f]{8}$`)

var toolSmithAsync = shellAsyncSpec{
	kind: asyncKindToolSmith, idPrefix: "smith_", idPattern: toolSmithRunID,
	noun: "Script", labelPrefix: "script: ", resultsDir: "tool_smith",
}

// startToolSmithAsync runs the script as a handle of the async manager and blocks up to
// blockMs for it.
func startToolSmithAsync(ctx context.Context, host *AsyncHost, script toolSmithScript, blockMs *int64) (string, error) {
	execution := script.execution
	execution.RequestInteraction = nil
	execution.OnInteractionResolved = nil
	script.execution = execution
	return startShellAsync(ctx, host, toolSmithAsync, time.Duration(script.settings.Child.KillGraceMs)*time.Millisecond, execution, shellLabel(toolSmithAsync, script.body), blockMs, func(runCtx context.Context) shellRunResult {
		return runToolSmithScript(runCtx, script)
	})
}
