package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"jad/backend-go/internal/app"
	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/sessionfs"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, errCheckFailed) {
			os.Exit(1)
		}
		// A startup or shutdown failure is always reported on stderr, whatever
		// server.log_level says: it explains why the process exited.
		fmt.Fprintf(os.Stderr, "jad: %v\n", err)
		os.Exit(1)
	}
}

// errCheckFailed is returned by "jad check" when it found a problem; the problems are
// already printed, so main exits 1 without adding a message.
var errCheckFailed = errors.New("check failed")

// runCommand runs "jad check": it reads the root setting and every catalog and session file
// as the server would, starts nothing, needs no API token, and fails when a file is invalid.
func runCommand(args []string, stdout io.Writer) error {
	if args[0] != "check" || len(args) > 1 {
		return fmt.Errorf("unknown arguments %q; the only command is \"check\"", args)
	}
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{})
	if err != nil {
		return fmt.Errorf("load server configuration: %w", err)
	}
	valid, err := app.CheckFiles(loaded.Roots, stdout)
	if err != nil {
		return err
	}
	if !valid {
		return errCheckFailed
	}
	return nil
}

func run() error {
	if len(os.Args) > 1 {
		return runCommand(os.Args[1:], os.Stdout)
	}
	shutdownSignal, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	config, err := app.LoadConfig(sessionfs.LoadOptions{})
	if err != nil {
		return fmt.Errorf("load server configuration: %w", err)
	}
	logger, closeLog, err := logging.New(config.Log, os.Stderr)
	if err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	defer closeLog.Close()
	config.HTTP.Logger = logger
	service, err := app.NewService(config)
	if err != nil {
		return fmt.Errorf("initialize Go backend: %w", err)
	}
	defer service.Close()

	address := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	server := config.NewHTTPServer(service.Handler)
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	logger.Info("Go agent API listening",
		slog.String("address", listener.Addr().String()),
		slog.String("log_level", config.Log.Level),
		slog.String("log_file", config.Log.File))

	select {
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			service.Close()
			return fmt.Errorf("serve HTTP requests: %w", err)
		}
		return nil
	case <-shutdownSignal.Done():
	}

	service.Close()
	if _, err := config.ShutdownHTTPServer(server, logger); err != nil {
		return fmt.Errorf("shut down HTTP server: %w", err)
	}
	if err := <-serveErrors; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP requests during shutdown: %w", err)
	}
	return nil
}
