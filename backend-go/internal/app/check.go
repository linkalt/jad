package app

import (
	"fmt"
	"io"

	"jad/backend-go/internal/sessionfs"
	mcpclient "jad/backend-go/internal/tools/mcp"
)

// CheckFiles reads every catalog and session file of the roots as the server would load it
// and writes one line per problem and a summary to w. It reports whether all files are valid.
func CheckFiles(roots []sessionfs.Root, w io.Writer) (bool, error) {
	report, err := sessionfs.CheckLayout(roots, mcpclient.ValidateServerFile)
	if err != nil {
		return false, err
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
