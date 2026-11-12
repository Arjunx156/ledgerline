// Package apierror is the one error shape the API returns (spec 3.8, FR-13):
// type, code, a message that says how to fix it, and the parameter at fault.
package apierror

import (
	"fmt"
	"net/http"
)

// Error types, as in the response's "type" field.
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeAuthentication = "authentication_error"
	TypeCard           = "card_error"
	TypeAPI            = "api_error"
)

// Error is both a Go error and the body of an error response.
type Error struct {
	Status  int    `json:"-"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// InvalidRequest is a 400: the request can't succeed as sent.
func InvalidRequest(code, param, message string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: TypeInvalidRequest, Code: code, Param: param, Message: message}
}

// Missing is a 400 for a required parameter that was not sent.
func Missing(param string) *Error {
	return InvalidRequest("parameter_missing", param, fmt.Sprintf("Missing required parameter: %s.", param))
}

// NoSuch is a 400 for a parameter naming an object that doesn't exist.
func NoSuch(param, object, id string) *Error {
	return InvalidRequest("resource_missing", param, fmt.Sprintf("No such %s: '%s'.", object, id))
}

// NotFound is a 404 for an object addressed in the path.
func NotFound(object, id string) *Error {
	return &Error{Status: http.StatusNotFound, Type: TypeInvalidRequest, Code: "resource_missing", Param: "id",
		Message: fmt.Sprintf("No such %s: '%s'.", object, id)}
}

// UnknownURL is a 404 for a path or method the API doesn't serve.
func UnknownURL(method, path string) *Error {
	return &Error{Status: http.StatusNotFound, Type: TypeInvalidRequest, Code: "url_unknown",
		Message: fmt.Sprintf("Unrecognized request URL (%s: %s).", method, path)}
}

// Conflict is a 409: the request clashes with one that came before it.
func Conflict(code, message string) *Error {
	return &Error{Status: http.StatusConflict, Type: TypeInvalidRequest, Code: code, Message: message}
}

// Authentication is a 401.
func Authentication(code, message string) *Error {
	return &Error{Status: http.StatusUnauthorized, Type: TypeAuthentication, Code: code, Message: message}
}

// Card is a 402: the request was valid but the card was declined.
func Card(code, message string) *Error {
	return &Error{Status: http.StatusPaymentRequired, Type: TypeCard, Code: code, Message: message}
}

// ProcessorUnavailable is a 503 (amendment C5). The payment stays where it
// was, and the processor dedupes on the request id, so whatever settles it
// later can't charge twice. Resuming by retrying the request comes with
// idempotency keys.
func ProcessorUnavailable() *Error {
	return &Error{Status: http.StatusServiceUnavailable, Type: TypeAPI, Code: "processor_unavailable",
		Message: "The card processor did not answer in time. The payment stays where it was until its outcome is known."}
}

// Internal is a 500. It never carries the underlying error, which is logged
// instead, so internals don't leak into responses.
func Internal(requestID string) *Error {
	return &Error{Status: http.StatusInternalServerError, Type: TypeAPI, Code: "internal_error",
		Message: fmt.Sprintf("Something went wrong on our side. If it keeps happening, quote request %s.", requestID)}
}
