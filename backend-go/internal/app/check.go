package app

import (
	"context"
	"fmt"
	"io"

	"jad/backend-go/internal/sessionfs"
	mcpclient "jad/backend-go/internal/tools/mcp"
	"jad/backend-go/internal/tools/native"
)

// CheckFiles reads every catalog and session file of the roots as the server would load it
// and writes one line per problem and a summary to w; it also starts each distinct enabled
// sandbox once. It reports whether all files are valid and every sandbox started.
func CheckFiles(roots []sessionfs.Root, w io.Writer) (bool, error) {
	report, err := sessionfs.CheckLayout(roots, mcpclient.ValidateServerFile)
	if err != nil {
		return false, err
	}
	// Files that load can still name a sandbox that does not start here; each distinct one is
	// tried once and reported against the first session that uses it.
	for _, sandbox := range report.Sandboxes {
		if err := native.ProbeSandbox(context.Background(), sandbox.Settings, sandbox.WorkingDir); err != nil {
			report.Problems = append(report.Problems, sessionfs.CheckProblem{Kind: "session", ID: sandbox.SessionID, Message: "sandbox: " + err.Error()})
		}
	}
	for _, problem := range report.Problems {
		if problem.ID == "" {
			fmt.Fprintf(w, "%s: %s\n", problem.Kind, problem.Message)
			continue
		}
		fmt.Fprintf(w, "%s %s: %s\n", problem.Kind, problem.ID, problem.Message)
	}
	fmt.Fprintf(w, "checked %d files, %d problems\n", report.Checked, len(report.Problems))
	return len(report.Problems) == 0, nil
}
