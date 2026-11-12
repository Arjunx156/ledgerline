package processor_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/database/dbtest"
	"github.com/Arjunx156/ledgerline/internal/processor"
)

// outlive is how long pm_card_timeout holds its answer. Tests give the
// caller a much shorter deadline.
const outlive = 5 * time.Second

func newSimulator(t *testing.T) (*processor.Simulator, *dbtest.DB) {
	t.Helper()
	db := dbtest.New(t)
	return processor.NewSimulator(db.App, outlive), db
}

func authorize(ctx context.Context, sim *processor.Simulator, requestID, pm string) (processor.Authorization, error) {
	return sim.Authorize(ctx, processor.AuthorizeRequest{
		RequestID: requestID, PaymentMethod: pm, Amount: 49900, Currency: "inr",
	})
}

func approved(t *testing.T, sim *processor.Simulator, requestID string) string {
	t.Helper()
	auth, err := authorize(t.Context(), sim, requestID, processor.TokenOK)
	require.NoError(t, err)
	require.True(t, auth.Approved)
	return auth.ID
}

func count(t *testing.T, db *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(t.Context(), query, args...).Scan(&n))
	return n
}

func requireRejected(t *testing.T, err error, reason string) {
	t.Helper()
	var rejected *processor.RejectedError
	require.ErrorAs(t, err, &rejected)
	assert.Contains(t, rejected.Reason, reason)
}

func TestAuthorizeTokens(t *testing.T) {
	sim, _ := newSimulator(t)
	tests := []struct {
		pm       string
		approved bool
		decline  string
	}{
		{processor.TokenOK, true, ""},
		{processor.TokenDeclined, false, "card_declined"},
		{processor.TokenInsufficientFunds, false, "insufficient_funds"},
	}
	for _, tt := range tests {
		t.Run(tt.pm, func(t *testing.T) {
			auth, err := authorize(t.Context(), sim, "pi_"+tt.pm+":authorize", tt.pm)
			require.NoError(t, err)
			assert.Equal(t, tt.approved, auth.Approved)
			assert.Equal(t, tt.decline, auth.DeclineCode)
			if tt.approved {
				assert.Regexp(t, `^auth_[A-Za-z0-9]{24}$`, auth.ID)
			} else {
				assert.Empty(t, auth.ID)
			}
		})
	}
}

func TestAuthorizeUnknownTokenIsRejected(t *testing.T) {
	sim, _ := newSimulator(t)
	_, err := authorize(t.Context(), sim, "pi_1:authorize", "pm_card_visa")
	requireRejected(t, err, "unknown payment method")
}

func TestFlakyFailsFirstCallThenDecides(t *testing.T) {
	sim, db := newSimulator(t)
	_, err := authorize(t.Context(), sim, "pi_1:authorize", processor.TokenFlaky)
	require.ErrorIs(t, err, processor.ErrUnavailable)
	assert.Zero(t, count(t, db.Owner, `SELECT count(*) FROM simulator.authorizations`),
		"an unavailable answer must not authorize anything")

	first, err := authorize(t.Context(), sim, "pi_1:authorize", processor.TokenFlaky)
	require.NoError(t, err)
	require.True(t, first.Approved)

	again, err := authorize(t.Context(), sim, "pi_1:authorize", processor.TokenFlaky)
	require.NoError(t, err)
	assert.Equal(t, first, again)
}

// pm_card_timeout approves, then never answers in time: the caller sees a
// timeout although the money was authorized. Asking again finds out.
func TestTimeoutApprovesButOutlivesTheCaller(t *testing.T) {
	sim, db := newSimulator(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := authorize(ctx, sim, "pi_1:authorize", processor.TokenTimeout)
	require.ErrorIs(t, err, processor.ErrUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, count(t, db.Owner, `SELECT count(*) FROM simulator.authorizations WHERE state = 'authorized'`),
		"the approval is committed before the caller gives up")

	start := time.Now()
	auth, err := authorize(t.Context(), sim, "pi_1:authorize", processor.TokenTimeout)
	require.NoError(t, err)
	assert.True(t, auth.Approved)
	assert.Less(t, time.Since(start), outlive, "a repeat answers at once")
	assert.Equal(t, 1, count(t, db.Owner, `SELECT count(*) FROM simulator.authorizations`))
}

// The dedupe lives in Postgres, so a new simulator, standing in for a
// restarted process, still knows every answer it gave (amendment C12).
func TestRepeatAfterRestartReturnsStoredOutcome(t *testing.T) {
	sim, db := newSimulator(t)
	first := approved(t, sim, "pi_1:authorize")

	restarted := processor.NewSimulator(db.App, outlive)
	auth, err := authorize(t.Context(), restarted, "pi_1:authorize", processor.TokenOK)
	require.NoError(t, err)
	assert.Equal(t, first, auth.ID)
	assert.Equal(t, 1, count(t, db.Owner, `SELECT count(*) FROM simulator.authorizations`))
	assert.Equal(t, 2, count(t, db.Owner, `SELECT calls FROM simulator.requests WHERE request_id = 'pi_1:authorize'`))
}

func TestConcurrentRepeatsAuthorizeOnce(t *testing.T) {
	sim, db := newSimulator(t)
	const callers = 20
	ids := make([]string, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			auth, err := authorize(t.Context(), sim, "pi_1:authorize", processor.TokenOK)
			assert.NoError(t, err)
			ids[i] = auth.ID
		})
	}
	wg.Wait()
	for _, id := range ids {
		assert.Equal(t, ids[0], id)
	}
	assert.Equal(t, 1, count(t, db.Owner, `SELECT count(*) FROM simulator.authorizations`))
	assert.Equal(t, callers, count(t, db.Owner, `SELECT calls FROM simulator.requests`))
}

func TestRequestIDReusedWithDifferentParameters(t *testing.T) {
	sim, _ := newSimulator(t)
	authID := approved(t, sim, "pi_1:authorize")

	_, err := sim.Authorize(t.Context(), processor.AuthorizeRequest{
		RequestID: "pi_1:authorize", PaymentMethod: processor.TokenOK, Amount: 100, Currency: "inr",
	})
	require.ErrorIs(t, err, processor.ErrRequestIDReused)

	err = sim.Capture(t.Context(), processor.CaptureRequest{RequestID: "pi_1:authorize", AuthorizationID: authID, Amount: 100})
	require.ErrorIs(t, err, processor.ErrRequestIDReused, "the operation is part of what a request id names")
}

func TestPartialCaptureThenNothingElse(t *testing.T) {
	sim, db := newSimulator(t)
	authID := approved(t, sim, "pi_1:authorize")

	require.NoError(t, sim.Capture(t.Context(), processor.CaptureRequest{
		RequestID: "pi_1:capture", AuthorizationID: authID, Amount: 30000,
	}))
	require.NoError(t, sim.Capture(t.Context(), processor.CaptureRequest{
		RequestID: "pi_1:capture", AuthorizationID: authID, Amount: 30000,
	}), "a repeated capture gets the original answer")
	assert.Equal(t, 30000, count(t, db.Owner, `SELECT amount_captured FROM simulator.authorizations WHERE id = $1`, authID))

	err := sim.Capture(t.Context(), processor.CaptureRequest{RequestID: "pi_1:capture-again", AuthorizationID: authID, Amount: 100})
	requireRejected(t, err, "already captured")
	err = sim.Void(t.Context(), processor.VoidRequest{RequestID: "pi_1:void", AuthorizationID: authID})
	requireRejected(t, err, "already captured")
}

func TestCaptureIsCappedAtTheAuthorizedAmount(t *testing.T) {
	sim, _ := newSimulator(t)
	authID := approved(t, sim, "pi_1:authorize")
	for _, amount := range []int64{0, 49901} {
		err := sim.Capture(t.Context(), processor.CaptureRequest{
			RequestID: fmt.Sprintf("pi_1:capture-%d", amount), AuthorizationID: authID, Amount: amount,
		})
		requireRejected(t, err, "outside 1..49900")
	}
	require.NoError(t, sim.Capture(t.Context(), processor.CaptureRequest{
		RequestID: "pi_1:capture", AuthorizationID: authID, Amount: 49900,
	}), "a refused capture leaves the authorization open")
}

func TestVoidThenNoCapture(t *testing.T) {
	sim, _ := newSimulator(t)
	authID := approved(t, sim, "pi_1:authorize")
	require.NoError(t, sim.Void(t.Context(), processor.VoidRequest{RequestID: "pi_1:void", AuthorizationID: authID}))
	err := sim.Capture(t.Context(), processor.CaptureRequest{RequestID: "pi_1:capture", AuthorizationID: authID, Amount: 49900})
	requireRejected(t, err, "already voided")
}

func TestUnknownAuthorizationIsRejected(t *testing.T) {
	sim, _ := newSimulator(t)
	err := sim.Capture(t.Context(), processor.CaptureRequest{RequestID: "pi_1:capture", AuthorizationID: "auth_missing", Amount: 1})
	requireRejected(t, err, "no such authorization")
}

func TestFlakyCaptureFailsFirstCall(t *testing.T) {
	sim, _ := newSimulator(t)
	_, err := authorize(t.Context(), sim, "pi_1:authorize", processor.TokenFlaky)
	require.ErrorIs(t, err, processor.ErrUnavailable)
	auth, err := authorize(t.Context(), sim, "pi_1:authorize", processor.TokenFlaky)
	require.NoError(t, err)

	req := processor.CaptureRequest{RequestID: "pi_1:capture", AuthorizationID: auth.ID, Amount: 49900}
	require.ErrorIs(t, sim.Capture(t.Context(), req), processor.ErrUnavailable)
	require.NoError(t, sim.Capture(t.Context(), req))
}

// A capture and a void racing on one authorization: the row lock decides
// them one after the other, so exactly one moves money.
func TestCaptureAndVoidRaceExactlyOneWins(t *testing.T) {
	sim, db := newSimulator(t)
	for i := range 25 {
		authID := approved(t, sim, fmt.Sprintf("pi_%d:authorize", i))
		var captureErr, voidErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			captureErr = sim.Capture(t.Context(), processor.CaptureRequest{
				RequestID: authID + ":capture", AuthorizationID: authID, Amount: 49900,
			})
		})
		wg.Go(func() {
			voidErr = sim.Void(t.Context(), processor.VoidRequest{RequestID: authID + ":void", AuthorizationID: authID})
		})
		wg.Wait()

		require.True(t, (captureErr == nil) != (voidErr == nil),
			"exactly one must win: capture=%v void=%v", captureErr, voidErr)
		loser := captureErr
		if loser == nil {
			loser = voidErr
		}
		var rejected *processor.RejectedError
		require.ErrorAs(t, loser, &rejected)

		var state string
		require.NoError(t, db.Owner.QueryRow(t.Context(),
			`SELECT state FROM simulator.authorizations WHERE id = $1`, authID).Scan(&state))
		if captureErr == nil {
			assert.Equal(t, "captured", state)
		} else {
			assert.Equal(t, "voided", state)
		}
	}
}

func TestProcessorFailuresSurfaceAsUnavailable(t *testing.T) {
	sim, db := newSimulator(t)
	db.App.Close()
	_, err := authorize(t.Context(), sim, "pi_1:authorize", processor.TokenOK)
	require.ErrorIs(t, err, processor.ErrUnavailable)
	err = sim.Capture(t.Context(), processor.CaptureRequest{RequestID: "pi_1:capture", AuthorizationID: "auth_x", Amount: 1})
	require.ErrorIs(t, err, processor.ErrUnavailable)
}
