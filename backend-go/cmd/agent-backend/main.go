package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"example.com/agent-harness/backend-go/internal/app"
	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	shutdownSignal, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	config, err := app.LoadConfig(sessionfs.LoadOptions{})
	if err != nil {
		return fmt.Errorf("load server configuration: %w", err)
	}
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
	server := &http.Server{Handler: service.Handler}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	log.Printf("Go agent API listening at http://%s", listener.Addr())

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
