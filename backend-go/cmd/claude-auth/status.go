package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"jad/backend-go/internal/claudeauth"
)

func runStatus(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	var asJSON *bool
	configPath, status, ok := parseFlags("status", args, stderr, func(set *flag.FlagSet) {
		asJSON = set.Bool("json", false, "print the report as JSON")
	})
	if !ok {
		return status
	}
	s, ok := load(*configPath, stderr)
	if !ok {
		return exitFailed
	}
	report := claudeauth.Inspect(s.store, s.config.ExpiryMargin(), now())
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "claude-auth status: %v\n", err)
			return exitFailed
		}
		return report.ExitCode()
	}
	printStatus(stdout, report, now())
	return report.ExitCode()
}

func printStatus(out io.Writer, report *claudeauth.Status, now time.Time) {
	fmt.Fprintf(out, "file:           %s\n", report.Path)
	if !report.Exists {
		fmt.Fprintf(out, "problem:        %s\n", report.Problem)
		return
	}
	mode := report.Mode
	if report.ModeTooOpen {
		mode += "  (readable by other users; run chmod 600)"
	}
	fmt.Fprintf(out, "mode:           %s\n", mode)
	if report.Problem != "" {
		fmt.Fprintf(out, "problem:        %s\n", report.Problem)
	}
	fmt.Fprintf(out, "scope:          %s\n", orNone(report.Scope))
	fmt.Fprintf(out, "access token:   %s\n", describeAccess(report, now))
	fmt.Fprintf(out, "refresh token:  %s\n", yesNo(report.RefreshToken))
	if report.LastRefresh != nil {
		fmt.Fprintf(out, "last refresh:   %s (%s ago)\n", report.LastRefresh.UTC().Format(time.RFC3339), report.SinceRefresh)
	} else {
		fmt.Fprintln(out, "last refresh:   unknown")
	}
	fmt.Fprintf(out, "backup file:    %s\n", yesNo(report.BackupExists))
	fmt.Fprintf(out, "usable by expiry: %s\n", yesNo(report.UsableByExpiry))
	fmt.Fprintln(out, "note: this reads the expiry stored at the last refresh. The server can reject a token before then.")
}

func describeAccess(report *claudeauth.Status, now time.Time) string {
	switch {
	case !report.AccessToken:
		return "none"
	case report.ExpiresAt == nil:
		return "present, no valid expiry stored"
	case report.Expired:
		return "EXPIRED (stored expiry " + report.ExpiresAt.UTC().Format(time.RFC3339) + ")"
	}
	return "expires in " + report.ExpiresAt.Sub(now).Round(time.Minute).String() + " (stored expiry)"
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}
