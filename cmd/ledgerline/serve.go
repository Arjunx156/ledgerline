package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Arjunx156/ledgerline/internal/api"
	"github.com/Arjunx156/ledgerline/internal/buildinfo"
	"github.com/Arjunx156/ledgerline/internal/database"
)

// How long in-flight requests get to finish after a shutdown signal.
const drainTimeout = 10 * time.Second

func serve(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	dbURL, err := requireEnv(getenv, "LEDGERLINE_DATABASE_URL")
	if err != nil {
		return err
	}
	pool, err := database.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	ln, err := net.Listen("tcp", cmp.Or(getenv("LEDGERLINE_ADDR"), ":8080"))
	if err != nil {
		return err
	}
	logger.Info("serving", "addr", ln.Addr().String(), "commit", buildinfo.Read().String())
	return runServer(ctx, ln, api.NewRouter(pool, logger), logger)
}

// runServer serves until ctx is cancelled, then stops accepting connections
// and lets in-flight requests finish, so a deploy never cuts a payment off
// halfway through a response.
func runServer(ctx context.Context, ln net.Listener, h http.Handler, logger *slog.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down", "drain_timeout", drainTimeout.String())
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("drain in-flight requests: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("stopped")
	return nil
}
