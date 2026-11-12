package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A panicking handler costs the client a JSON 500 naming the request, not
// a dropped connection, and the stack goes to the log.
func TestPanicBecomesA500(t *testing.T) {
	var logs bytes.Buffer
	h := instrument(slog.New(slog.NewJSONHandler(&logs, nil)))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var body struct{ Error map[string]string }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "internal_error", body.Error["code"])
	assert.Contains(t, body.Error["message"], rec.Header().Get("Request-Id"))
	assert.NotContains(t, rec.Body.String(), "boom", "internals stay in the log")
	assert.Contains(t, logs.String(), "boom")
	assert.Contains(t, logs.String(), `"status":500`)
}

func TestRedact(t *testing.T) {
	assert.Equal(t, "sk_test_****wxyz", redact("sk_test_abcdefghijklmnopqrstuvwxyz"))
	assert.Equal(t, "****wxyz", redact("not_a_key_wxyz"))
	assert.Equal(t, "****", redact("short"))
}
