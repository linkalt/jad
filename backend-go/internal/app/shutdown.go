package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
)

// ShutdownHTTPServer stops server from taking new connections and waits for in-flight
// requests, at most Config.ShutdownTimeout (zero waits without a limit). When the time runs
// out the remaining connections are closed and forced is true; that is not an error.
func (c Config) ShutdownHTTPServer(server *http.Server, logger *slog.Logger) (forced bool, err error) {
	ctx := context.Background()
	if c.ShutdownTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.ShutdownTimeout)
		defer cancel()
	}
	err = server.Shutdown(ctx)
	if err == nil {
		return false, nil
	}
	closeErr := server.Close()
	if errors.Is(err, context.DeadlineExceeded) {
		if logger != nil {
			logger.Warn("shutdown timed out; remaining connections were closed", slog.Duration("shutdown_timeout", c.ShutdownTimeout))
		}
		return true, closeErr
	}
	return false, err
}
