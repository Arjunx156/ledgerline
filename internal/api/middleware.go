package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/apierror"
	"github.com/Arjunx156/ledgerline/internal/auth"
	"github.com/Arjunx156/ledgerline/internal/ids"
	"github.com/Arjunx156/ledgerline/internal/logctx"
	"github.com/Arjunx156/ledgerline/internal/trace"
)

type requestIDKey struct{}
type merchantKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func merchantFrom(ctx context.Context) string {
	id, _ := ctx.Value(merchantKey{}).(string)
	return id
}

// instrument gives every request an id (returned as Request-Id), a logger
// that carries it, one access log line, a trace of its start and end, and a
// JSON 500 instead of a dropped connection if a handler panics.
func instrument(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := ids.New(ids.Request)
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			ctx = logctx.New(trace.WithRequest(ctx, id), logger, "request_id", id)
			r = r.WithContext(ctx)
			w.Header().Set("Request-Id", id)
			rec := &statusRecorder{ResponseWriter: w}

			trace.Emit(ctx, trace.LaneAPI, "api.request", trace.Fields{Data: map[string]any{
				"method": r.Method, "path": r.URL.Path,
			}})
			defer func() {
				if p := recover(); p != nil {
					if p == http.ErrAbortHandler {
						panic(p)
					}
					logctx.From(ctx).ErrorContext(ctx, "handler panicked", "panic", p, "stack", string(debug.Stack()))
					if rec.status == 0 {
						writeError(rec, r, apierror.Internal(id))
					}
				}
				trace.Emit(ctx, trace.LaneAPI, "api.response", trace.Fields{Data: map[string]any{"status": rec.status}})
				logctx.From(ctx).InfoContext(ctx, "request", "method", r.Method, "path", r.URL.Path,
					"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// authenticate resolves the Bearer key to a merchant (FR-1). Operator keys
// are refused here: they open /internal only, so a leaked operator key can't
// move a merchant's money and a merchant key can't read the ledger.
func authenticate(db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearer(r)
			if !ok {
				writeError(w, r, apierror.Authentication("api_key_missing",
					"No API key provided. Send your secret key as: Authorization: Bearer sk_test_..."))
				return
			}
			key, err := auth.Lookup(r.Context(), db, raw)
			switch {
			case errors.Is(err, auth.ErrUnknownKey):
				writeError(w, r, apierror.Authentication("api_key_invalid", "Invalid API key provided: "+redact(raw)+"."))
				return
			case err != nil:
				writeError(w, r, err)
				return
			case key.Operator:
				writeError(w, r, apierror.Authentication("api_key_invalid",
					"This is an operator key, which only opens /internal. Use a merchant secret key for /v1."))
				return
			}
			logctx.Add(r.Context(), "merchant_id", key.MerchantID)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), merchantKey{}, key.MerchantID)))
		})
	}
}

func bearer(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// redact shows only a key's last four characters, enough to tell keys
// apart in an error without echoing a secret back.
func redact(raw string) string {
	if len(raw) <= 8 {
		return "****"
	}
	prefix := ""
	if strings.HasPrefix(raw, auth.KeyPrefix) {
		prefix = auth.KeyPrefix
	}
	return prefix + "****" + raw[len(raw)-4:]
}
