// Command anthropic-adapter serves the OpenAI Responses API and forwards the
// calls to the Anthropic Messages API. Point a model's base_url at it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"jad/backend-go/internal/anthropicadapter"
)

const shutdownGrace = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "anthropic-adapter: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to the adapter JSON config (defaults are used when empty)")
	flag.Parse()
	config, err := anthropicadapter.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", config.Listen, err)
	}
	handler := anthropicadapter.NewHandler(config, nil)
	handler.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: shutdownGrace,
	}
	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	fmt.Fprintf(os.Stderr, "anthropic-adapter: listening on %s, upstream %s\n", listener.Addr(), config.Upstream.BaseURL)

	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalContext.Done():
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return server.Shutdown(shutdownContext)
}
