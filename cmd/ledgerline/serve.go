package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/api"
	"github.com/Arjunx156/ledgerline/internal/buildinfo"
	"github.com/Arjunx156/ledgerline/internal/database"
	"github.com/Arjunx156/ledgerline/internal/payments"
	"github.com/Arjunx156/ledgerline/internal/processor"
)

const (
	// How long in-flight requests get to finish after a shutdown signal.
	drainTimeout = 10 * time.Second

	// How long the API waits on each processor call before answering 503.
	processorTimeout = 5 * time.Second

	// How long pm_card_timeout holds its answer: well past processorTimeout,
	// so the API always gives up first, as it would on a real network.
	simulatorOutlive = 30 * time.Second
)

func serve(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	app, err := start(ctx, "serving", getenv, logger)
	if err != nil {
		return err
	}
	defer app.pool.Close()
	return runServer(ctx, app.ln, api.NewRouter(app.config(logger)), logger)
}

// app is what serve and demo share: the database, the listener and the
// payments service.
type app struct {
	pool     *pgxpool.Pool
	ln       net.Listener
	payments *payments.Service
}

func start(ctx context.Context, mode string, getenv func(string) string, logger *slog.Logger) (*app, error) {
	dbURL, err := requireEnv(getenv, "LEDGERLINE_DATABASE_URL")
	if err != nil {
		return nil, err
	}
	pool, err := database.Open(ctx, dbURL)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", listenAddr(getenv))
	if err != nil {
		pool.Close()
		return nil, err
	}
	logger.Info(mode, "addr", ln.Addr().String(), "commit", buildinfo.Read().String())
	sim := processor.NewSimulator(pool, simulatorOutlive)
	svc := payments.NewService(pool, sim, payments.Config{ProcessorTimeout: processorTimeout})
	return &app{pool: pool, ln: ln, payments: svc}, nil
}

func (a *app) config(logger *slog.Logger) api.Config {
	return api.Config{DB: a.pool, Logger: logger, Payments: a.payments}
}

// listenAddr is LEDGERLINE_ADDR, else the PORT a hosting platform assigns,
// else :8080.
func listenAddr(getenv func(string) string) string {
	if addr := getenv("LEDGERLINE_ADDR"); addr != "" {
		return addr
	}
	if port := getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
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
