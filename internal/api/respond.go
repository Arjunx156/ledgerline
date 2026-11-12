package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/Arjunx156/ledgerline/internal/apierror"
	"github.com/Arjunx156/ledgerline/internal/logctx"
	"github.com/Arjunx156/ledgerline/internal/payments"
)

// handler is an endpoint that returns the object to send, or an error.
// Every success is a 200 with the full object (spec 3.8).
type handler func(r *http.Request) (any, error)

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v, err := h(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends an *apierror.Error as is. Anything else is a bug or an
// outage: it is logged in full and the client gets a 500 that names the
// request, never the cause.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) {
		logctx.From(r.Context()).ErrorContext(r.Context(), "request failed", "err", err)
		apiErr = apierror.Internal(requestIDFrom(r.Context()))
	}
	if apiErr.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, apiErr.Status, struct {
		Error *apierror.Error `json:"error"`
	}{apiErr})
}

const maxBody = 64 << 10

// decode reads a JSON object into dst, a pointer to a struct of pointer
// fields. It is strict, so a typo is a 400 that names the parameter rather
// than a silently ignored field, and 49900.5 or 4.99e4 are not amounts.
func decode(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if len(body) > maxBody {
		return apierror.InvalidRequest("body_too_large", "", "Request bodies are limited to 64 KB.")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return apierror.InvalidRequest("body_invalid", "", "The request body must be a JSON object.")
	}
	known := jsonNames(dst)
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if !known[name] {
			return apierror.InvalidRequest("parameter_unknown", name, "Received unknown parameter: "+name+".")
		}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return typeError(typeErr)
		}
		return apierror.InvalidRequest("body_invalid", "", "The request body must be a JSON object.")
	}
	return nil
}

func typeError(e *json.UnmarshalTypeError) error {
	param := e.Field
	switch e.Type.Kind() {
	case reflect.Int64:
		return apierror.InvalidRequest("parameter_invalid_integer", param,
			param+" must be a whole number, in the currency's minor unit.")
	case reflect.String:
		return apierror.InvalidRequest("parameter_invalid_string", param, param+" must be a string.")
	}
	return apierror.InvalidRequest("parameter_invalid", param, param+" has the wrong type.")
}

func jsonNames(dst any) map[string]bool {
	t := reflect.TypeOf(dst).Elem()
	names := make(map[string]bool, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		names[name] = true
	}
	return names
}

// page reads limit and starting_after (FR-12). Any other query parameter
// is refused, as in bodies.
func page(r *http.Request) (payments.Page, error) {
	q := r.URL.Query()
	for _, name := range slices.Sorted(maps.Keys(q)) {
		if name != "limit" && name != "starting_after" {
			return payments.Page{}, apierror.InvalidRequest("parameter_unknown", name, "Received unknown parameter: "+name+".")
		}
	}
	p := payments.Page{Limit: 10, StartingAfter: q.Get("starting_after")}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			return payments.Page{}, apierror.InvalidRequest("parameter_invalid_integer", "limit", "limit must be a whole number.")
		}
		if n < 1 || n > 100 {
			return payments.Page{}, apierror.InvalidRequest("parameter_invalid", "limit", "limit must be between 1 and 100.")
		}
		p.Limit = n
	}
	return p, nil
}
