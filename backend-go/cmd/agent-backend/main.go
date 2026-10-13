package main

import (
	"context"
	"errors"
	"fmt"
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
		// A startup or shutdown failure is always reported on stderr, whatever
		// server.log_level says: it explains why the process exited.
		fmt.Fprintf(os.Stderr, "agent-backend: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
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
	if err := server.Shutdown(context.Background()); err != nil {
		_ = server.Close()
		return fmt.Errorf("shut down HTTP server: %w", err)
	}
	if err := <-serveErrors; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP requests during shutdown: %w", err)
	}
	return nil
}
