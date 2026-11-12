// Package processor is the card network as Ledgerline sees it, and a
// simulator that plays its part (spec 3.7, amendment C4 and C12).
package processor

import (
	"context"
	"errors"
)

// Processor is the other side of the network. Every call carries a request
// id the processor dedupes on, so repeating a call is always safe: the
// second call gets the first call's answer, never a second charge.
type Processor interface {
	Authorize(ctx context.Context, req AuthorizeRequest) (Authorization, error)
	Capture(ctx context.Context, req CaptureRequest) error
	Void(ctx context.Context, req VoidRequest) error
}

type AuthorizeRequest struct {
	RequestID     string
	PaymentMethod string
	Amount        int64
	Currency      string
}

// Authorization is the answer to Authorize: approved with an id that
// capture and void reference, or declined with a reason.
type Authorization struct {
	ID          string
	Approved    bool
	DeclineCode string
}

type CaptureRequest struct {
	RequestID       string
	AuthorizationID string
	Amount          int64
}

type VoidRequest struct {
	RequestID       string
	AuthorizationID string
}

var (
	// ErrUnavailable is a processor 5xx or a timeout. The outcome is unknown
	// to the caller; retrying with the same request id finds it out.
	ErrUnavailable = errors.New("processor unavailable")

	// ErrRequestIDReused means the request id was seen before with
	// different parameters: a bug in the caller, never retried.
	ErrRequestIDReused = errors.New("processor request id reused with different parameters")
)

// RejectedError is the processor refusing what its own records forbid,
// such as capturing a voided authorization.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return "processor rejected the request: " + e.Reason }

// Test payment method tokens (spec 3.7). No card numbers exist anywhere.
const (
	TokenOK                = "pm_card_ok"
	TokenDeclined          = "pm_card_declined"
	TokenInsufficientFunds = "pm_card_insufficient_funds"
	TokenTimeout           = "pm_card_timeout"
	TokenFlaky             = "pm_card_flaky"
)

// KnownToken reports whether the simulator knows a payment method.
func KnownToken(pm string) bool {
	switch pm {
	case TokenOK, TokenDeclined, TokenInsufficientFunds, TokenTimeout, TokenFlaky:
		return true
	}
	return false
}
