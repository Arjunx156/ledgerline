package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/auth"
)

func (s *stack) createIntent(t *testing.T, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"amount": 49900, "currency": "inr"}
	for k, v := range extra {
		body[k] = v
	}
	return s.ok(t, "POST", "/v1/payment_intents", body)["id"].(string)
}

// Spec section 4's worked example, steps 1, 2 and 5. Steps 3 and 4 replay
// a confirm by its Idempotency-Key, which arrives with stage S3.
func TestWorkedExample(t *testing.T) {
	s := newStack(t, nil, nil)

	created := s.ok(t, "POST", "/v1/payment_intents", map[string]any{"amount": 49900, "currency": "inr"})
	assert.Equal(t, "requires_confirmation", created["status"])
	assert.EqualValues(t, 0, created["amount_captured"])

	id := created["id"].(string)
	confirmed := s.ok(t, "POST", "/v1/payment_intents/"+id+"/confirm", map[string]any{"payment_method": "pm_card_ok"})
	assert.Equal(t, "succeeded", confirmed["status"])
	assert.EqualValues(t, 49900, confirmed["amount_captured"])

	declined := s.createIntent(t, nil)
	r := s.do(t, "POST", "/v1/payment_intents/"+declined+"/confirm", map[string]any{"payment_method": "pm_card_declined"})
	assert.Equal(t, http.StatusPaymentRequired, r.status)
	assert.Equal(t, "card_error", r.errorField("type"))
	assert.Equal(t, "card_declined", r.errorField("code"))
	assert.Equal(t, "failed", s.ok(t, "GET", "/v1/payment_intents/"+declined, nil)["status"])
}

func TestPaymentIntentObjectShape(t *testing.T) {
	s := newStack(t, nil, nil)
	pi := s.ok(t, "POST", "/v1/payment_intents", map[string]any{"amount": 100, "currency": "inr", "capture_method": "manual"})
	keys := make([]string, 0, len(pi))
	for k := range pi {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"id", "object", "amount", "amount_captured", "amount_refunded", "capture_method",
		"created", "currency", "customer", "failure_code", "payment_method", "status"}, keys)
	assert.Equal(t, "payment_intent", pi["object"])
	assert.Nil(t, pi["customer"], "absent values are null, not missing")
	assert.Greater(t, pi["created"].(float64), float64(1.7e9), "created is Unix seconds")
}

func TestManualCaptureAndCancelOverHTTP(t *testing.T) {
	s := newStack(t, nil, nil)
	customer := s.ok(t, "POST", "/v1/customers", map[string]any{"email": "asha@example.com"})["id"]

	id := s.createIntent(t, map[string]any{"capture_method": "manual", "customer": customer})
	assert.Equal(t, "requires_capture",
		s.ok(t, "POST", "/v1/payment_intents/"+id+"/confirm", map[string]any{"payment_method": "pm_card_ok"})["status"])
	r := s.do(t, "POST", "/v1/payment_intents/"+id+"/capture", map[string]any{"amount_to_capture": 50000})
	assert.Equal(t, http.StatusBadRequest, r.status)
	assert.Equal(t, "amount_too_large", r.errorField("code"))
	assert.Equal(t, "amount_to_capture", r.errorField("param"))

	captured := s.ok(t, "POST", "/v1/payment_intents/"+id+"/capture", map[string]any{"amount_to_capture": 30000})
	assert.Equal(t, "succeeded", captured["status"])
	assert.EqualValues(t, 30000, captured["amount_captured"])
	assert.Equal(t, customer, captured["customer"])

	r = s.do(t, "POST", "/v1/payment_intents/"+id+"/cancel", nil)
	assert.Equal(t, http.StatusBadRequest, r.status)
	assert.Equal(t, "payment_intent_unexpected_state", r.errorField("code"))
	assert.Contains(t, r.errorField("message"), "status is succeeded")

	other := s.createIntent(t, nil)
	assert.Equal(t, "canceled", s.ok(t, "POST", "/v1/payment_intents/"+other+"/cancel", map[string]any{})["status"])
}

func TestProcessorUnavailableIs503WithRetryAfter(t *testing.T) {
	s := newStack(t, nil, nil)
	id := s.createIntent(t, nil)
	r := s.do(t, "POST", "/v1/payment_intents/"+id+"/confirm", map[string]any{"payment_method": "pm_card_timeout"})
	assert.Equal(t, http.StatusServiceUnavailable, r.status)
	assert.Equal(t, "1", r.header.Get("Retry-After"))
	assert.Equal(t, "api_error", r.errorField("type"))
	assert.Equal(t, "processor_unavailable", r.errorField("code"))
	assert.Equal(t, "processing", s.ok(t, "GET", "/v1/payment_intents/"+id, nil)["status"])
}

func TestAuthentication(t *testing.T) {
	s := newStack(t, nil, nil)
	operatorKey := auth.NewKey()
	_, err := s.db.Owner.Exec(t.Context(), `INSERT INTO api_keys (key_hash, merchant_id, operator) VALUES ($1, $2, true)`,
		auth.Hash(operatorKey), s.merchant)
	require.NoError(t, err)
	wrongKey := auth.NewKey()

	tests := []struct {
		name          string
		authorization string
		code          string
	}{
		{"no header", "", "api_key_missing"},
		{"not bearer", "Basic " + s.key, "api_key_missing"},
		{"empty bearer", "Bearer ", "api_key_missing"},
		{"unknown key", "Bearer " + wrongKey, "api_key_invalid"},
		{"operator key", "Bearer " + operatorKey, "api_key_invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := s.doWithAuth(t, "GET", "/v1/payment_intents", nil, tt.authorization)
			assert.Equal(t, http.StatusUnauthorized, r.status)
			assert.Equal(t, "authentication_error", r.errorField("type"))
			assert.Equal(t, tt.code, r.errorField("code"))
			assert.NotContains(t, string(r.raw), wrongKey, "a key is never echoed back in full")
		})
	}
	r := s.doWithAuth(t, "GET", "/v1/payment_intents", nil, "bearer "+s.key)
	assert.Equal(t, http.StatusOK, r.status, "the scheme is case-insensitive")
	assert.Contains(t, s.doWithAuth(t, "GET", "/v1/customers", nil, "Bearer "+wrongKey).errorField("message"),
		"sk_test_****"+wrongKey[len(wrongKey)-4:])
}

// The Build Guide's by-hand checks, and the other ways a request can be
// malformed. Each error names what was wrong and the parameter at fault.
func TestErrors(t *testing.T) {
	s := newStack(t, nil, nil)
	tests := []struct {
		name, method, path string
		body               any
		status             int
		code, param        string
	}{
		{"negative amount", "POST", "/v1/payment_intents", map[string]any{"amount": -100, "currency": "inr"}, 400, "amount_invalid", "amount"},
		{"amount too large", "POST", "/v1/payment_intents", map[string]any{"amount": 100_000_000, "currency": "inr"}, 400, "amount_too_large", "amount"},
		{"fractional amount", "POST", "/v1/payment_intents", `{"amount": 49900.5, "currency": "inr"}`, 400, "parameter_invalid_integer", "amount"},
		{"exponent amount", "POST", "/v1/payment_intents", `{"amount": 4.99e4, "currency": "inr"}`, 400, "parameter_invalid_integer", "amount"},
		{"string amount", "POST", "/v1/payment_intents", `{"amount": "49900", "currency": "inr"}`, 400, "parameter_invalid_integer", "amount"},
		{"numeric currency", "POST", "/v1/payment_intents", `{"amount": 1, "currency": 356}`, 400, "parameter_invalid_string", "currency"},
		{"missing amount", "POST", "/v1/payment_intents", map[string]any{"currency": "inr"}, 400, "parameter_missing", "amount"},
		{"missing currency", "POST", "/v1/payment_intents", map[string]any{"amount": 1}, 400, "parameter_missing", "currency"},
		{"misspelt parameter", "POST", "/v1/payment_intents", map[string]any{"amount": 1, "currency": "inr", "amout": 1}, 400, "parameter_unknown", "amout"},
		{"not json", "POST", "/v1/customers", `email=asha@example.com`, 400, "body_invalid", ""},
		{"json array", "POST", "/v1/customers", `[1, 2]`, 400, "body_invalid", ""},
		{"body too large", "POST", "/v1/customers", `{"name": "` + strings.Repeat("a", 70_000) + `"}`, 400, "body_too_large", ""},
		{"unknown payment intent", "GET", "/v1/payment_intents/pi_missing", nil, 404, "resource_missing", "id"},
		{"unknown customer", "GET", "/v1/customers/cus_missing", nil, 404, "resource_missing", "id"},
		{"confirm unknown payment intent", "POST", "/v1/payment_intents/pi_missing/confirm", map[string]any{"payment_method": "pm_card_ok"}, 404, "resource_missing", "id"},
		{"unknown payment method", "POST", "/v1/payment_intents/pi_missing/confirm", map[string]any{"payment_method": "pm_card_visa"}, 400, "resource_missing", "payment_method"},
		{"limit zero", "GET", "/v1/payment_intents?limit=0", nil, 400, "parameter_invalid", "limit"},
		{"limit words", "GET", "/v1/customers?limit=ten", nil, 400, "parameter_invalid_integer", "limit"},
		{"unknown cursor", "GET", "/v1/customers?starting_after=cus_missing", nil, 400, "resource_missing", "starting_after"},
		{"unknown query parameter", "GET", "/v1/customers?email=asha@example.com", nil, 400, "parameter_unknown", "email"},
		{"unknown url", "GET", "/v1/charges", nil, 404, "url_unknown", ""},
		{"unknown method", "DELETE", "/v1/customers", nil, 404, "url_unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := s.do(t, tt.method, tt.path, tt.body)
			require.Equal(t, tt.status, r.status, "%s", r.raw)
			assert.Equal(t, tt.code, r.errorField("code"))
			e := r.body["error"].(map[string]any)
			assert.NotEmpty(t, e["message"])
			if tt.param == "" {
				assert.NotContains(t, e, "param")
			} else {
				assert.Equal(t, tt.param, e["param"])
			}
			for k := range e {
				assert.Contains(t, []string{"type", "code", "message", "param"}, k, "spec 3.8 allows four fields")
			}
			assert.Regexp(t, `^req_[A-Za-z0-9]{24}$`, r.header.Get("Request-Id"))
		})
	}
}

func TestListCustomersOverHTTP(t *testing.T) {
	s := newStack(t, nil, nil)
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		s.ok(t, "POST", "/v1/customers", map[string]any{"email": email})
	}
	first := s.ok(t, "GET", "/v1/customers?limit=2", nil)
	assert.Equal(t, "list", first["object"])
	assert.Equal(t, "/v1/customers", first["url"])
	assert.Equal(t, true, first["has_more"])
	data := first["data"].([]any)
	require.Len(t, data, 2)
	assert.Equal(t, "c@example.com", data[0].(map[string]any)["email"], "newest first")

	last := data[1].(map[string]any)["id"].(string)
	rest := s.ok(t, "GET", "/v1/customers?limit=2&starting_after="+last, nil)
	assert.Equal(t, false, rest["has_more"])
	assert.Equal(t, "a@example.com", rest["data"].([]any)[0].(map[string]any)["email"])

	empty := s.ok(t, "GET", "/v1/payment_intents", nil)
	assert.Equal(t, []any{}, empty["data"], "an empty list is [], not null")
}

// Every log line of a request names it, its merchant and its payment
// (spec 2.4), including the access log line written after the handler.
func TestAccessLogCarriesRequestMerchantAndPayment(t *testing.T) {
	var logs bytes.Buffer
	s := newStack(t, nil, &logs)
	id := s.createIntent(t, nil)
	r := s.do(t, "GET", "/v1/payment_intents/"+id, nil)

	var line map[string]any
	for sc := bufio.NewScanner(bytes.NewReader(logs.Bytes())); sc.Scan(); {
		var l map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &l))
		if l["request_id"] == r.header.Get("Request-Id") && l["msg"] == "request" {
			line = l
		}
	}
	require.NotNil(t, line, "no access log line for the request:\n%s", logs.String())
	assert.Equal(t, s.merchant, line["merchant_id"])
	assert.Equal(t, id, line["payment_id"])
	assert.EqualValues(t, 200, line["status"])
	assert.NotContains(t, logs.String(), s.key, "keys never reach the logs")
}
