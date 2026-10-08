package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"example.com/agent-harness/backend-go/internal/openaiauth"
)

func runStatus(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	var asJSON *bool
	_, configPath, status, ok := parseFlags("status", args, stderr, func(set *flag.FlagSet) {
		asJSON = set.Bool("json", false, "print the report as JSON")
	})
	if !ok {
		return status
	}
	s, ok := load(*configPath, stderr)
	if !ok {
		return exitFailed
	}
	report := openaiauth.Inspect(s.store, s.config.ExpiryMargin(), now())
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "openai-auth status: %v\n", err)
			return exitFailed
		}
		return report.ExitCode()
	}
	printStatus(stdout, report, now())
	return report.ExitCode()
}

func printStatus(out io.Writer, report *openaiauth.Status, now time.Time) {
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
	fmt.Fprintf(out, "auth_mode:      %s\n", orNone(report.AuthMode))
	fmt.Fprintf(out, "account id:     %s\n", shorten(report.AccountID))
	if report.FedRAMP {
		fmt.Fprintln(out, "fedramp:        yes")
	}
	fmt.Fprintf(out, "refresh token:  %s\n", yesNo(report.RefreshToken))
	if report.LastRefresh != nil {
		fmt.Fprintf(out, "last refresh:   %s (%s ago)\n", report.LastRefresh.UTC().Format(time.RFC3339), report.SinceRefresh)
	} else {
		fmt.Fprintln(out, "last refresh:   unknown")
	}
	printToken(out, "access token", report.AccessToken, now)
	printToken(out, "id token", report.IDToken, now)
	fmt.Fprintf(out, "backup file:    %s\n", yesNo(report.BackupExists))
	fmt.Fprintf(out, "usable by claims: %s\n", yesNo(report.UsableByClaims))
	fmt.Fprintln(out, "note: this reads the tokens' own expiry claims. The server can reject a token whose claim is still valid.")
}

func printToken(out io.Writer, label string, token openaiauth.TokenStatus, now time.Time) {
	label += ":"
	switch {
	case !token.Present:
		fmt.Fprintf(out, "%-15s none\n", label)
	case !token.Readable:
		fmt.Fprintf(out, "%-15s present, not a JWT (no times known)\n", label)
	default:
		fmt.Fprintf(out, "%-15s %s\n", label, describeTimes(token, now))
	}
}

func describeTimes(token openaiauth.TokenStatus, now time.Time) string {
	text := ""
	if token.IssuedAt != nil {
		text += "issued " + now.Sub(*token.IssuedAt).Round(time.Minute).String() + " ago"
	}
	if token.ExpiresAt != nil {
		if text != "" {
			text += ", "
		}
		left := token.ExpiresAt.Sub(now).Round(time.Minute)
		if token.Expired {
			text += "EXPIRED (claim says " + token.ExpiresAt.UTC().Format(time.RFC3339) + ")"
		} else {
			text += "expires in " + left.String() + " (claim)"
		}
	}
	if text == "" {
		return "present, no issued/expiry claims"
	}
	return text
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

// shorten keeps the first and last characters of an id, enough to tell accounts apart.
func shorten(id string) string {
	switch {
	case id == "":
		return "(none)"
	case len(id) <= 12:
		return id
	}
	return id[:8] + "…" + id[len(id)-4:]
}
