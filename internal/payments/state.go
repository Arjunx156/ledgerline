// Package payments holds payment intents, the customers they belong to, and
// the state machine every status change goes through (spec 3.3).
package payments

import (
	"fmt"

	"github.com/Arjunx156/ledgerline/internal/apierror"
)

// Status is where a payment intent is in its life.
type Status string

const (
	StatusRequiresConfirmation Status = "requires_confirmation"
	StatusProcessing           Status = "processing"
	StatusRequiresCapture      Status = "requires_capture"
	StatusSucceeded            Status = "succeeded"
	StatusFailed               Status = "failed"
	StatusCanceled             Status = "canceled"
)

// statusNew is the state before a payment intent exists.
const statusNew Status = ""

// Event is what happens to a payment intent.
type Event string

const (
	EventCreate           Event = "create"
	EventConfirm          Event = "confirm"
	EventCancel           Event = "cancel"
	EventAuthorized       Event = "authorized"        // capture_method=automatic, and the capture went through
	EventAuthorizedManual Event = "authorized_manual" // capture_method=manual
	EventDeclined         Event = "declined"
	EventProcessorTimeout Event = "processor_timeout"
	EventCapture          Event = "capture"
	EventRefund           Event = "refund"
)

type edge struct {
	from  Status
	event Event
}

// transitions is the table in spec 3.3, row for row. Every status change in
// Ledgerline is decided here and nowhere else.
var transitions = map[edge]Status{
	{statusNew, EventCreate}:                   StatusRequiresConfirmation,
	{StatusRequiresConfirmation, EventConfirm}: StatusProcessing,
	{StatusRequiresConfirmation, EventCancel}:  StatusCanceled,
	{StatusProcessing, EventAuthorized}:        StatusSucceeded,
	{StatusProcessing, EventAuthorizedManual}:  StatusRequiresCapture,
	{StatusProcessing, EventDeclined}:          StatusFailed,
	{StatusProcessing, EventProcessorTimeout}:  StatusProcessing, // the recovery path re-asks the processor
	{StatusRequiresCapture, EventCapture}:      StatusSucceeded,
	{StatusRequiresCapture, EventCancel}:       StatusCanceled, // the authorization is voided first
	{StatusSucceeded, EventRefund}:             StatusSucceeded,
}

// Next returns the status ev moves a payment intent in status from to, or a
// 400 payment_intent_unexpected_state if the table has no such row.
func Next(from Status, ev Event) (Status, error) {
	if to, ok := transitions[edge{from, ev}]; ok {
		return to, nil
	}
	return "", unexpectedState(fmt.Sprintf("This PaymentIntent's status is %s, which does not allow %s.", from, ev))
}

func unexpectedState(message string) *apierror.Error {
	return apierror.InvalidRequest("payment_intent_unexpected_state", "", message)
}
