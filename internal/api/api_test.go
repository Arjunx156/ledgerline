package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/api"
	"github.com/Arjunx156/ledgerline/internal/database/dbtest"
)

func getHealthz(t *testing.T, h http.Handler) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	return rec.Code, body
}

func TestHealthz(t *testing.T) {
	db := dbtest.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	code, body := getHealthz(t, api.NewRouter(api.Config{DB: db.App, Logger: logger}))
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ok", body["status"])
	assert.NotEmpty(t, body["commit"])

	db.App.Close()
	code, body = getHealthz(t, api.NewRouter(api.Config{DB: db.App, Logger: logger}))
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "database_unavailable", body["status"])
}
