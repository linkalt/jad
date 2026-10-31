// Package logging builds the structured logger that the Go backend injects into
// its components. It wraps log/slog, owns no global state, and never logs
// request or response bodies, headers, tokens or prompts: callers pass only
// identifiers, route templates, statuses, durations and error causes.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"jad/backend-go/internal/serverconfig"
)

// Settings selects the level, encoding and destination of the logger. The zero
// value means the defaults: info level, text format, standard error.
type Settings struct {
	// Level is debug, info, warn, error or off.
	Level string
	// Format is text or json.
	Format string
	// File is an append-only log file; empty writes to the standard error stream.
	File string
}

// discard is immutable and shared: it holds no state and drops every record.
var discard = slog.New(slog.DiscardHandler)

// Discard returns a logger that drops every record without formatting it.
func Discard() *slog.Logger { return discard }

// Or returns logger, or a discarding logger when logger is nil, so components can
// treat an unset logger as "logging disabled".
func Or(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return Discard()
	}
	return logger
}

// New builds a logger from settings. stderr receives the records when
// settings.File is empty. The returned closer releases the log file and is a
// no-op otherwise. An unusable log file is an error: a configured destination
// that silently drops records would hide the failure it was meant to expose.
func New(settings Settings, stderr io.Writer) (*slog.Logger, io.Closer, error) {
	level := settings.Level
	if level == "" {
		level = serverconfig.DefaultLogLevel
	}
	format := settings.Format
	if format == "" {
		format = serverconfig.DefaultLogFormat
	}
	if !serverconfig.IsLogLevel(level) {
		return nil, nil, fmt.Errorf("log level %q is not one of debug, info, warn, error, off", level)
	}
	if !serverconfig.IsLogFormat(format) {
		return nil, nil, fmt.Errorf("log format %q is not one of text, json", format)
	}
	if level == "off" {
		return Discard(), noopCloser{}, nil
	}

	var output io.Writer = stderr
	var closer io.Closer = noopCloser{}
	if settings.File != "" {
		if err := os.MkdirAll(filepath.Dir(settings.File), 0o700); err != nil {
			return nil, nil, fmt.Errorf("create log directory for %s: %w", settings.File, err)
		}
		file, err := os.OpenFile(settings.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		// OpenFile applies the mode only when it creates the file; an existing log keeps its old mode.
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			return nil, nil, fmt.Errorf("set log file mode: %w", err)
		}
		output, closer = file, file
	}
	options := &slog.HandlerOptions{Level: slogLevel(level)}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(output, options)
	} else {
		handler = slog.NewTextHandler(output, options)
	}
	return slog.New(handler), closer, nil
}

func slogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
