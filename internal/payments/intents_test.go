package payments

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/apierror"
	"github.com/Arjunx156/ledgerline/internal/auth"
	"github.com/Arjunx156/ledgerline/internal/database/dbtest"
	"github.com/Arjunx156/ledgerline/internal/processor"
)

// counting wraps the simulator so tests can see which calls were made.
type counting struct {
	processor.Processor
	authorizes, captures, voids atomic.Int32
}

func (c *counting) Authorize(ctx context.Context, r processor.AuthorizeRequest) (processor.Authorization, error) {
	c.authorizes.Add(1)
	return c.Processor.Authorize(ctx, r)
}

func (c *counting) Capture(ctx context.Context, r processor.CaptureRequest) error {
	c.captures.Add(1)
	return c.Processor.Capture(ctx, r)
}

func (c *counting) Void(ctx context.Context, r processor.VoidRequest) error {
	c.voids.Add(1)
	return c.Processor.Void(ctx, r)
}

type fixture struct {
	db       *dbtest.DB
	svc      *Service
	proc     *counting
	merchant string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := dbtest.New(t)
	proc := &counting{Processor: processor.NewSimulator(db.App, 5*time.Second)}
	merchant, _, err := auth.CreateMerchant(t.Context(), db.App, "Test merchant")
	require.NoError(t, err)
	return &fixture{
		db:       db,
		svc:      NewService(db.App, proc, Config{ProcessorTimeout: time.Second}),
		proc:     proc,
		merchant: merchant,
	}
}

func (f *fixture) create(t *testing.T, captureMethod string) PaymentIntent {
	t.Helper()
	pi, err := f.svc.Create(t.Context(), f.merchant, CreateParams{Amount: 49900, Currency: "inr", CaptureMethod: captureMethod})
	require.NoError(t, err)
	return pi
}

func (f *fixture) authorization(t *testing.T, id string) (state string, captured int64) {
	t.Helper()
	require.NoError(t, f.db.Owner.QueryRow(t.Context(),
		`SELECT state, amount_captured FROM simulator.authorizations WHERE id = $1`, id).Scan(&state, &captured))
	return state, captured
}

func requireAPIError(t *testing.T, err error, status int, code, param string) {
	t.Helper()
	var apiErr *apierror.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, status, apiErr.Status, apiErr.Message)
	assert.Equal(t, code, apiErr.Code, apiErr.Message)
	assert.Equal(t, param, apiErr.Param)
}

func TestCreateStartsInRequiresConfirmation(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, "")
	assert.Equal(t, StatusRequiresConfirmation, pi.Status)
	assert.Equal(t, CaptureAutomatic, pi.CaptureMethod)
	assert.Equal(t, 1, pi.Version)
	assert.Zero(t, pi.AmountCaptured)
	assert.Regexp(t, `^pi_[A-Za-z0-9]{24}$`, pi.ID)
}

func TestCreateValidates(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name  string
		p     CreateParams
		code  string
		param string
	}{
		{"zero amount", CreateParams{Amount: 0, Currency: "inr"}, "amount_invalid", "amount"},
		{"negative amount", CreateParams{Amount: -500, Currency: "inr"}, "amount_invalid", "amount"},
		{"amount above the cap", CreateParams{Amount: MaxAmount + 1, Currency: "inr"}, "amount_too_large", "amount"},
		{"currency not a code", CreateParams{Amount: 1, Currency: "rupees"}, "parameter_invalid", "currency"},
		{"capture method unknown", CreateParams{Amount: 1, Currency: "inr", CaptureMethod: "later"}, "parameter_invalid", "capture_method"},
		{"customer unknown", CreateParams{Amount: 1, Currency: "inr", CustomerID: "cus_missing"}, "resource_missing", "customer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Create(t.Context(), f.merchant, tt.p)
			requireAPIError(t, err, 400, tt.code, tt.param)
		})
	}

	pi, err := f.svc.Create(t.Context(), f.merchant, CreateParams{Amount: MaxAmount, Currency: "INR"})
	require.NoError(t, err, "the cap itself is allowed")
	assert.Equal(t, "inr", pi.Currency)
}

func TestConfirmAutomaticCapturesInFull(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, CaptureAutomatic)

	pi, err := f.svc.Confirm(t.Context(), f.merchant, pi.ID, processor.TokenOK)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, pi.Status)
	assert.Equal(t, int64(49900), pi.AmountCaptured)
	assert.Equal(t, 3, pi.Version, "create, then processing, then succeeded")
	require.NotNil(t, pi.AuthorizationID)
	state, captured := f.authorization(t, *pi.AuthorizationID)
	assert.Equal(t, "captured", state)
	assert.Equal(t, int64(49900), captured)
}

func TestConfirmDeclined(t *testing.T) {
	for _, pm := range []string{processor.TokenDeclined, processor.TokenInsufficientFunds} {
		t.Run(pm, func(t *testing.T) {
			f := newFixture(t)
			pi := f.create(t, "")
			_, err := f.svc.Confirm(t.Context(), f.merchant, pi.ID, pm)
			code := map[string]string{processor.TokenDeclined: "card_declined", processor.TokenInsufficientFunds: "insufficient_funds"}[pm]
			requireAPIError(t, err, 402, code, "")

			pi, err = f.svc.Get(t.Context(), f.merchant, pi.ID)
			require.NoError(t, err)
			assert.Equal(t, StatusFailed, pi.Status)
			require.NotNil(t, pi.FailureCode)
			assert.Equal(t, code, *pi.FailureCode)
			assert.Zero(t, pi.AmountCaptured)
			assert.Zero(t, f.proc.captures.Load())
		})
	}
}

// A processor that doesn't answer leaves the payment in processing and the
// client with a 503 to retry, never a guess at the outcome (amendment C5).
func TestConfirmProcessorUnavailableStaysProcessing(t *testing.T) {
	for _, pm := range []string{processor.TokenTimeout, processor.TokenFlaky} {
		t.Run(pm, func(t *testing.T) {
			f := newFixture(t)
			pi := f.create(t, "")
			_, err := f.svc.Confirm(t.Context(), f.merchant, pi.ID, pm)
			requireAPIError(t, err, 503, "processor_unavailable", "")

			pi, err = f.svc.Get(t.Context(), f.merchant, pi.ID)
			require.NoError(t, err)
			assert.Equal(t, StatusProcessing, pi.Status)
			assert.Nil(t, pi.AuthorizationID)
		})
	}
}

func TestConfirmUnknownPaymentMethod(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, "")
	_, err := f.svc.Confirm(t.Context(), f.merchant, pi.ID, "pm_card_visa")
	requireAPIError(t, err, 400, "resource_missing", "payment_method")
	_, err = f.svc.Confirm(t.Context(), f.merchant, pi.ID, "")
	requireAPIError(t, err, 400, "parameter_missing", "payment_method")

	pi, err = f.svc.Get(t.Context(), f.merchant, pi.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRequiresConfirmation, pi.Status)
	assert.Zero(t, f.proc.authorizes.Load())
}

func TestManualCaptureOfPart(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, CaptureManual)
	pi, err := f.svc.Confirm(t.Context(), f.merchant, pi.ID, processor.TokenOK)
	require.NoError(t, err)
	assert.Equal(t, StatusRequiresCapture, pi.Status)
	assert.Zero(t, pi.AmountCaptured)
	assert.Zero(t, f.proc.captures.Load())

	amount := int64(30000)
	pi, err = f.svc.Capture(t.Context(), f.merchant, pi.ID, &amount)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, pi.Status)
	assert.Equal(t, int64(30000), pi.AmountCaptured)
	state, captured := f.authorization(t, *pi.AuthorizationID)
	assert.Equal(t, "captured", state)
	assert.Equal(t, int64(30000), captured, "the remaining 19900 is released")
}

func TestCaptureAmountBounds(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, CaptureManual)
	pi, err := f.svc.Confirm(t.Context(), f.merchant, pi.ID, processor.TokenOK)
	require.NoError(t, err)

	for amount, code := range map[int64]string{0: "amount_invalid", -1: "amount_invalid", 49901: "amount_too_large"} {
		_, err := f.svc.Capture(t.Context(), f.merchant, pi.ID, &amount)
		requireAPIError(t, err, 400, code, "amount_to_capture")
	}
	assert.Zero(t, f.proc.captures.Load(), "a bad amount never reaches the processor")

	pi, err = f.svc.Capture(t.Context(), f.merchant, pi.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(49900), pi.AmountCaptured, "no amount captures everything")
}

func TestCancelBeforeConfirmMakesNoProcessorCall(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, "")
	pi, err := f.svc.Cancel(t.Context(), f.merchant, pi.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCanceled, pi.Status)
	assert.Zero(t, f.proc.voids.Load()+f.proc.authorizes.Load())
}

func TestCancelAfterAuthorizationVoids(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, CaptureManual)
	pi, err := f.svc.Confirm(t.Context(), f.merchant, pi.ID, processor.TokenOK)
	require.NoError(t, err)

	pi, err = f.svc.Cancel(t.Context(), f.merchant, pi.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCanceled, pi.Status)
	state, _ := f.authorization(t, *pi.AuthorizationID)
	assert.Equal(t, "voided", state)
}

func TestIllegalRequestsAreRefusedBeforeTheProcessor(t *testing.T) {
	f := newFixture(t)
	failed := f.create(t, "")
	_, err := f.svc.Confirm(t.Context(), f.merchant, failed.ID, processor.TokenDeclined)
	require.Error(t, err)
	succeeded := f.create(t, "")
	_, err = f.svc.Confirm(t.Context(), f.merchant, succeeded.ID, processor.TokenOK)
	require.NoError(t, err)
	fresh := f.create(t, CaptureManual)
	before := f.proc.captures.Load() + f.proc.voids.Load()

	_, err = f.svc.Capture(t.Context(), f.merchant, failed.ID, nil)
	requireAPIError(t, err, 400, "payment_intent_unexpected_state", "")
	_, err = f.svc.Capture(t.Context(), f.merchant, succeeded.ID, nil)
	requireAPIError(t, err, 400, "payment_intent_unexpected_state", "")
	_, err = f.svc.Capture(t.Context(), f.merchant, fresh.ID, nil)
	requireAPIError(t, err, 400, "payment_intent_unexpected_state", "")
	_, err = f.svc.Cancel(t.Context(), f.merchant, succeeded.ID)
	requireAPIError(t, err, 400, "payment_intent_unexpected_state", "")
	_, err = f.svc.Confirm(t.Context(), f.merchant, succeeded.ID, processor.TokenOK)
	requireAPIError(t, err, 400, "payment_intent_unexpected_state", "")
	assert.Equal(t, before, f.proc.captures.Load()+f.proc.voids.Load())
}

func TestAnotherMerchantsObjectsDoNotExist(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, "")
	customer, err := f.svc.CreateCustomer(t.Context(), f.merchant, nil, nil)
	require.NoError(t, err)
	other, _, err := auth.CreateMerchant(t.Context(), f.db.App, "Other merchant")
	require.NoError(t, err)

	_, err = f.svc.Get(t.Context(), other, pi.ID)
	requireAPIError(t, err, 404, "resource_missing", "id")
	_, err = f.svc.Confirm(t.Context(), other, pi.ID, processor.TokenOK)
	requireAPIError(t, err, 404, "resource_missing", "id")
	_, err = f.svc.GetCustomer(t.Context(), other, customer.ID)
	requireAPIError(t, err, 404, "resource_missing", "id")
	_, err = f.svc.Create(t.Context(), other, CreateParams{Amount: 1, Currency: "inr", CustomerID: customer.ID})
	requireAPIError(t, err, 400, "resource_missing", "customer")
	_, _, err = f.svc.List(t.Context(), other, Page{Limit: 10, StartingAfter: pi.ID})
	requireAPIError(t, err, 400, "resource_missing", "starting_after")
}

// Two requests that read the same version can't both win: a confirm and a
// cancel racing on a new payment leave exactly one of them standing.
func TestConfirmAndCancelRaceExactlyOneWins(t *testing.T) {
	f := newFixture(t)
	for range 20 {
		pi := f.create(t, "")
		var confirmErr, cancelErr error
		var wg sync.WaitGroup
		wg.Go(func() { _, confirmErr = f.svc.Confirm(t.Context(), f.merchant, pi.ID, processor.TokenOK) })
		wg.Go(func() { _, cancelErr = f.svc.Cancel(t.Context(), f.merchant, pi.ID) })
		wg.Wait()
		require.True(t, (confirmErr == nil) != (cancelErr == nil), "confirm=%v cancel=%v", confirmErr, cancelErr)

		got, err := f.svc.Get(t.Context(), f.merchant, pi.ID)
		require.NoError(t, err)
		if confirmErr == nil {
			assert.Equal(t, StatusSucceeded, got.Status)
		} else {
			assert.Equal(t, StatusCanceled, got.Status)
			assert.Nil(t, got.AuthorizationID)
		}
	}
}

// A capture and a cancel racing on an authorized payment: the processor
// lets only one through, and the payment ends where the money did.
func TestCaptureAndCancelRaceAgreeWithTheProcessor(t *testing.T) {
	f := newFixture(t)
	for range 20 {
		pi := f.create(t, CaptureManual)
		pi, err := f.svc.Confirm(t.Context(), f.merchant, pi.ID, processor.TokenOK)
		require.NoError(t, err)
		var captureErr, cancelErr error
		var wg sync.WaitGroup
		wg.Go(func() { _, captureErr = f.svc.Capture(t.Context(), f.merchant, pi.ID, nil) })
		wg.Go(func() { _, cancelErr = f.svc.Cancel(t.Context(), f.merchant, pi.ID) })
		wg.Wait()
		require.True(t, (captureErr == nil) != (cancelErr == nil), "capture=%v cancel=%v", captureErr, cancelErr)

		got, err := f.svc.Get(t.Context(), f.merchant, pi.ID)
		require.NoError(t, err)
		state, captured := f.authorization(t, *pi.AuthorizationID)
		if captureErr == nil {
			assert.Equal(t, StatusSucceeded, got.Status)
			assert.Equal(t, "captured", state)
			assert.Equal(t, got.AmountCaptured, captured)
		} else {
			assert.Equal(t, StatusCanceled, got.Status)
			assert.Equal(t, "voided", state)
		}
	}
}

func TestApplyRetriesWhenOnlyTheVersionMoved(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, "")
	_, err := f.db.Owner.Exec(t.Context(), `UPDATE payment_intents SET version = version + 1 WHERE id = $1`, pi.ID)
	require.NoError(t, err)

	got, err := f.svc.apply(t.Context(), f.db.App, pi, EventCancel, change{})
	require.NoError(t, err)
	assert.Equal(t, StatusCanceled, got.Status)
	assert.Equal(t, 3, got.Version)
}

func TestApplyRefusesWhenTheStatusMoved(t *testing.T) {
	f := newFixture(t)
	pi := f.create(t, "")
	_, err := f.svc.Cancel(t.Context(), f.merchant, pi.ID)
	require.NoError(t, err)

	_, err = f.svc.apply(t.Context(), f.db.App, pi, EventConfirm, change{})
	requireAPIError(t, err, 400, "payment_intent_unexpected_state", "")
	assert.ErrorContains(t, err, "changed to canceled while this request was in flight")
}
