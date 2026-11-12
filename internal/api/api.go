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

	"github.com/Arjunx156/ledgerline/internal/buildinfo"
)

// NewRouter wires every route the API serves.
func NewRouter(db *pgxpool.Pool, logger *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", healthz(db, logger))
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
