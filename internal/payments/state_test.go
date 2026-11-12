package payments

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/apierror"
)

var (
	allStatuses = []Status{statusNew, StatusRequiresConfirmation, StatusProcessing, StatusRequiresCapture,
		StatusSucceeded, StatusFailed, StatusCanceled}
	allEvents = []Event{EventCreate, EventConfirm, EventCancel, EventAuthorized, EventAuthorizedManual,
		EventDeclined, EventProcessorTimeout, EventCapture, EventRefund}
)

// The spec's table, written out again by hand rather than read from the
// code, so a change to either one fails this test.
var specTable = []struct {
	from Status
	ev   Event
	to   Status
}{
	{statusNew, EventCreate, StatusRequiresConfirmation},
	{StatusRequiresConfirmation, EventConfirm, StatusProcessing},
	{StatusRequiresConfirmation, EventCancel, StatusCanceled},
	{StatusProcessing, EventAuthorized, StatusSucceeded},
	{StatusProcessing, EventAuthorizedManual, StatusRequiresCapture},
	{StatusProcessing, EventDeclined, StatusFailed},
	{StatusProcessing, EventProcessorTimeout, StatusProcessing},
	{StatusRequiresCapture, EventCapture, StatusSucceeded},
	{StatusRequiresCapture, EventCancel, StatusCanceled},
	{StatusSucceeded, EventRefund, StatusSucceeded},
}

func TestEveryRowOfTheSpecTable(t *testing.T) {
	for _, row := range specTable {
		t.Run(string(row.from)+"+"+string(row.ev), func(t *testing.T) {
			to, err := Next(row.from, row.ev)
			require.NoError(t, err)
			assert.Equal(t, row.to, to)
		})
	}
	assert.Len(t, transitions, len(specTable), "the code has a transition the spec doesn't")
}

// Every pair the table doesn't list, such as capturing a failed payment, is
// refused with the spec's error.
func TestEveryOtherPairIsRefused(t *testing.T) {
	legal := map[edge]bool{}
	for _, row := range specTable {
		legal[edge{row.from, row.ev}] = true
	}
	refused := 0
	for _, from := range allStatuses {
		for _, ev := range allEvents {
			if legal[edge{from, ev}] {
				continue
			}
			refused++
			to, err := Next(from, ev)
			var apiErr *apierror.Error
			require.ErrorAs(t, err, &apiErr, "%q + %q", from, ev)
			assert.Equal(t, "payment_intent_unexpected_state", apiErr.Code)
			assert.Equal(t, 400, apiErr.Status)
			assert.Empty(t, to)
		}
	}
	assert.Equal(t, len(allStatuses)*len(allEvents)-len(specTable), refused)
}

func TestTerminalStatusesAcceptOnlyRefunds(t *testing.T) {
	for _, from := range []Status{StatusFailed, StatusCanceled} {
		for _, ev := range allEvents {
			_, err := Next(from, ev)
			assert.Error(t, err, "%s is terminal but accepted %s", from, ev)
		}
	}
	_, err := Next(StatusSucceeded, EventCapture)
	assert.Error(t, err, "a succeeded payment can't be captured twice")
}

func TestRefusalNamesTheStatus(t *testing.T) {
	_, err := Next(StatusFailed, EventCapture)
	assert.EqualError(t, err, "payment_intent_unexpected_state: This PaymentIntent's status is failed, which does not allow capture.")
}
