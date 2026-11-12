// Package api is Ledgerline's HTTP surface.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/apierror"
	"github.com/Arjunx156/ledgerline/internal/buildinfo"
	"github.com/Arjunx156/ledgerline/internal/payments"
)

// Config is what the router needs.
type Config struct {
	DB       *pgxpool.Pool
	Logger   *slog.Logger
	Payments *payments.Service
}

// NewRouter wires every route the API serves (amendment C8).
func NewRouter(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Use(instrument(cfg.Logger))
	r.NotFound(handler(func(r *http.Request) (any, error) {
		return nil, apierror.UnknownURL(r.Method, r.URL.Path)
	}).ServeHTTP)
	r.MethodNotAllowed(handler(func(r *http.Request) (any, error) {
		return nil, apierror.UnknownURL(r.Method, r.URL.Path)
	}).ServeHTTP)

	r.Get("/healthz", healthz(cfg.DB, cfg.Logger))

	h := handlers{svc: cfg.Payments}
	r.Route("/v1", func(r chi.Router) {
		r.Use(authenticate(cfg.DB))
		r.Method(http.MethodPost, "/customers", handler(h.createCustomer))
		r.Method(http.MethodGet, "/customers", handler(h.listCustomers))
		r.Method(http.MethodGet, "/customers/{id}", handler(h.getCustomer))
		r.Method(http.MethodPost, "/payment_intents", handler(h.createPaymentIntent))
		r.Method(http.MethodGet, "/payment_intents", handler(h.listPaymentIntents))
		r.Method(http.MethodGet, "/payment_intents/{id}", handler(h.getPaymentIntent))
		r.Method(http.MethodPost, "/payment_intents/{id}/confirm", handler(h.confirmPaymentIntent))
		r.Method(http.MethodPost, "/payment_intents/{id}/capture", handler(h.capturePaymentIntent))
		r.Method(http.MethodPost, "/payment_intents/{id}/cancel", handler(h.cancelPaymentIntent))
	})
	return r
}

type health struct {
	Status string `json:"status"`
	Commit string `json:"commit"`
}

// healthz reports whether the database answers and which commit is running;
// the console uses the commit to link trace steps to source.
func healthz(db *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
	commit := buildinfo.Read().String()
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		body, code := health{Status: "ok", Commit: commit}, http.StatusOK
		if err := db.Ping(ctx); err != nil {
			logger.WarnContext(ctx, "healthz: database ping failed", "err", err)
			body.Status, code = "database_unavailable", http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
}
