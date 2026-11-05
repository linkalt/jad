package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"jad/backend-go/internal/sessionlock"
	"jad/backend-go/internal/transcript"
)

const transcriptUsage = `usage: jad transcript strip-steer [--dry-run] <messages.jsonl or folder>...

strip-steer prepares transcripts for a jad build from before the steering marker: it blanks
the "__agent_session_input_steer" field of every record that carries it, in place (the file
keeps its size and every record its offset). A folder is searched for files named
messages.jsonl. A transcript in use by a run is left alone and reported. Stop the server
before you run it; a run started afterwards marks new records again.`

// runTranscriptCommand runs "jad transcript".
func runTranscriptCommand(args []string, streams commandStreams) error {
	if len(args) == 0 || args[0] != "strip-steer" {
		return &exitError{code: exitFailed, err: errors.New(transcriptUsage)}
	}
	flags := flag.NewFlagSet("jad transcript strip-steer", flag.ContinueOnError)
	flags.SetOutput(streams.stderr)
	dryRun := flags.Bool("dry-run", false, "count the marked records without changing a file")
	if err := flags.Parse(args[1:]); err != nil {
		return &exitError{code: exitFailed}
	}
	if flags.NArg() == 0 {
		return &exitError{code: exitFailed, err: errors.New(transcriptUsage)}
	}
	files, err := transcriptFiles(flags.Args())
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	verb := "stripped"
	if *dryRun {
		verb = "would strip"
	}
	records, changed, failed := 0, 0, 0
	for _, path := range files {
		count, err := stripTranscript(path, !*dryRun)
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(streams.stderr, "jad: %v\n", err)
			continue
		}
		if count > 0 {
			records += count
			changed++
			_, _ = fmt.Fprintf(streams.stdout, "%s: %s %d marked records\n", path, verb, count)
		}
	}
	_, _ = fmt.Fprintf(streams.stdout, "%d transcripts read, %s %d marked records in %d\n", len(files)-failed, verb, records, changed)
	if failed > 0 {
		return &exitError{code: exitFailed, err: fmt.Errorf("%d transcripts were not changed", failed)}
	}
	return nil
}

// transcriptFiles expands the arguments: a file is taken as it is, a folder is searched for
// files named messages.jsonl.
func transcriptFiles(args []string) ([]string, error) {
	var files []string
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, arg)
			continue
		}
		err = filepath.WalkDir(arg, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() && entry.Name() == "messages.jsonl" {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// stripTranscript strips one transcript under its lock, which a run holds while it writes.
func stripTranscript(path string, write bool) (int, error) {
	lockPath, err := sessionlock.PathFor(path)
	if err != nil {
		return 0, err
	}
	lock, err := sessionlock.TryAcquire(context.Background(), lockPath)
	if errors.Is(err, sessionlock.ErrBusy) {
		return 0, fmt.Errorf("%s is in use by a run; stop the server and try again", path)
	}
	if err != nil {
		return 0, fmt.Errorf("lock %s: %w", path, err)
	}
	defer lock.Close()
	return transcript.StripSteerMarks(path, write)
}
