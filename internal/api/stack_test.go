package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/api"
	"github.com/Arjunx156/ledgerline/internal/auth"
	"github.com/Arjunx156/ledgerline/internal/database/dbtest"
	"github.com/Arjunx156/ledgerline/internal/payments"
	"github.com/Arjunx156/ledgerline/internal/processor"
	"github.com/Arjunx156/ledgerline/internal/trace"
)

// stack is the whole API over real HTTP and a real database, with one
// merchant and its key. When rec is set, the test plays the client and
// records both sides of every exchange.
type stack struct {
	db       *dbtest.DB
	url      string
	key      string
	merchant string
	rec      trace.Recorder
}

func newStack(t *testing.T, rec trace.Recorder, logs io.Writer) *stack {
	t.Helper()
	db := dbtest.New(t)
	if logs == nil {
		logs = io.Discard
	}
	svc := payments.NewService(db.App, processor.NewSimulator(db.App, 10*time.Second),
		payments.Config{ProcessorTimeout: time.Second})
	router := api.NewRouter(api.Config{DB: db.App, Logger: slog.New(slog.NewJSONHandler(logs, nil)), Payments: svc})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			r = r.WithContext(trace.WithRecorder(r.Context(), rec))
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	merchant, key, err := auth.CreateMerchant(t.Context(), db.App, "Test merchant")
	require.NoError(t, err)
	return &stack{db: db, url: srv.URL, key: key, merchant: merchant, rec: rec}
}

type response struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

// errorField returns one field of an error response.
func (r response) errorField(name string) any {
	e, _ := r.body["error"].(map[string]any)
	return e[name]
}

func (s *stack) do(t *testing.T, method, path string, body any) response {
	t.Helper()
	return s.doWithAuth(t, method, path, body, "Bearer "+s.key)
}

// doWithAuth sends body as JSON, or as is when it is a string, so tests can
// send malformed JSON.
func (s *stack) doWithAuth(t *testing.T, method, path string, body any, authorization string) response {
	t.Helper()
	var payload []byte
	switch b := body.(type) {
	case nil:
	case string:
		payload = []byte(b)
	default:
		var err error
		payload, err = json.Marshal(b)
		require.NoError(t, err)
	}
	ctx := context.Background()
	if s.rec != nil {
		ctx = trace.WithRecorder(ctx, s.rec)
		trace.Emit(ctx, trace.LaneClient, "client.request", trace.Fields{Data: map[string]any{
			"method": method, "path": path, "body": body,
		}})
	}

	req, err := http.NewRequestWithContext(ctx, method, s.url+path, bytes.NewReader(payload))
	require.NoError(t, err)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := response{status: resp.StatusCode, header: resp.Header, raw: raw}
	require.NoError(t, json.Unmarshal(raw, &out.body), "every response is JSON: %s", raw)

	if s.rec != nil {
		ctx = trace.WithRequest(ctx, resp.Header.Get("Request-Id"))
		trace.Emit(ctx, trace.LaneClient, "client.response", trace.Fields{Data: map[string]any{
			"status": resp.StatusCode, "body": out.body,
		}})
	}
	return out
}

// ok sends a request that must succeed and returns its body.
func (s *stack) ok(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	r := s.do(t, method, path, body)
	require.Equal(t, http.StatusOK, r.status, "%s %s: %s", method, path, r.raw)
	return r.body
}
