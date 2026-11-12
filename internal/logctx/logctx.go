// Package logctx carries a request's logger, so every line a request writes
// names its request id, merchant, idempotency key and payment (spec 2.4),
// however deep in the call stack it is written.
package logctx

import (
	"context"
	"log/slog"
	"sync"
)

type key struct{}

// The attrs are shared by pointer so that facts learned deep in a request,
// such as the payment id, also appear on the access log line written at the end.
type holder struct {
	mu    sync.Mutex
	base  *slog.Logger
	attrs []any
}

// New starts a request's logger.
func New(ctx context.Context, base *slog.Logger, args ...any) context.Context {
	return context.WithValue(ctx, key{}, &holder{base: base, attrs: args})
}

// Add attaches key-value pairs to every later line of this request.
func Add(ctx context.Context, args ...any) {
	if h, ok := ctx.Value(key{}).(*holder); ok {
		h.mu.Lock()
		h.attrs = append(h.attrs, args...)
		h.mu.Unlock()
	}
}

// From returns the request's logger, or the default logger outside a request.
func From(ctx context.Context) *slog.Logger {
	h, ok := ctx.Value(key{}).(*holder)
	if !ok {
		return slog.Default()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.base.With(h.attrs...)
}
