package payments

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/apierror"
	"github.com/Arjunx156/ledgerline/internal/ids"
	"github.com/Arjunx156/ledgerline/internal/processor"
	"github.com/Arjunx156/ledgerline/internal/trace"
)

// MaxAmount keeps amount × fee basis points far inside int64 (amendment C9).
const MaxAmount = 99_999_999

const (
	CaptureAutomatic = "automatic"
	CaptureManual    = "manual"
)

// PaymentIntent is one attempt to collect an amount from a customer.
type PaymentIntent struct {
	ID              string
	MerchantID      string
	CustomerID      *string
	Amount          int64
	Currency        string
	CaptureMethod   string
	Status          Status
	PaymentMethod   *string
	AuthorizationID *string
	AmountCaptured  int64
	AmountRefunded  int64
	FailureCode     *string
	Version         int
	CreatedAt       time.Time
}

// Config tunes the service.
type Config struct {
	// ProcessorTimeout bounds each processor call. Past it the payment stays
	// where it was and the client gets a 503 it can safely retry.
	ProcessorTimeout time.Duration
}

// Service runs payment intents against Postgres and a processor.
type Service struct {
	db   *pgxpool.Pool
	proc processor.Processor
	cfg  Config
}

func NewService(db *pgxpool.Pool, proc processor.Processor, cfg Config) *Service {
	return &Service{db: db, proc: proc, cfg: cfg}
}

// querier is a pool or a transaction. Later stages run apply inside their
// phase transactions; this stage runs it on the pool.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const intentColumns = `id, merchant_id, customer_id, amount, currency, capture_method, status, payment_method,
	authorization_id, amount_captured, amount_refunded, failure_code, version, created_at`

func scanIntent(row pgx.Row) (PaymentIntent, error) {
	var pi PaymentIntent
	err := row.Scan(&pi.ID, &pi.MerchantID, &pi.CustomerID, &pi.Amount, &pi.Currency, &pi.CaptureMethod,
		&pi.Status, &pi.PaymentMethod, &pi.AuthorizationID, &pi.AmountCaptured, &pi.AmountRefunded,
		&pi.FailureCode, &pi.Version, &pi.CreatedAt)
	return pi, err
}

// CreateParams are a new payment intent's parameters. CustomerID and
// CaptureMethod may be empty.
type CreateParams struct {
	Amount        int64
	Currency      string
	CustomerID    string
	CaptureMethod string
}

var currencyShape = regexp.MustCompile(`^[a-z]{3}$`)

func (p *CreateParams) validate() error {
	switch {
	case p.Amount < 1:
		return apierror.InvalidRequest("amount_invalid", "amount",
			"Amount must be a positive integer in the currency's minor unit, such as 49900 for ₹499.00.")
	case p.Amount > MaxAmount:
		return apierror.InvalidRequest("amount_too_large", "amount",
			fmt.Sprintf("Amount must be at most %d in the currency's minor unit.", MaxAmount))
	}
	p.Currency = strings.ToLower(p.Currency)
	if !currencyShape.MatchString(p.Currency) {
		return apierror.InvalidRequest("parameter_invalid", "currency",
			"Currency must be a three-letter ISO code, such as inr.")
	}
	switch p.CaptureMethod {
	case "":
		p.CaptureMethod = CaptureAutomatic
	case CaptureAutomatic, CaptureManual:
	default:
		return apierror.InvalidRequest("parameter_invalid", "capture_method",
			"capture_method must be automatic or manual.")
	}
	return nil
}

// Create makes a payment intent in requires_confirmation.
func (s *Service) Create(ctx context.Context, merchantID string, p CreateParams) (PaymentIntent, error) {
	if err := p.validate(); err != nil {
		return PaymentIntent{}, err
	}
	status, err := Next(statusNew, EventCreate)
	if err != nil {
		return PaymentIntent{}, err
	}
	var customerID *string
	if p.CustomerID != "" {
		customerID = &p.CustomerID
	}
	pi, err := scanIntent(s.db.QueryRow(ctx, `INSERT INTO payment_intents
		(id, merchant_id, customer_id, amount, currency, capture_method, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+intentColumns,
		ids.New(ids.PaymentIntent), merchantID, customerID, p.Amount, p.Currency, p.CaptureMethod, status))
	// The composite foreign key also stops a merchant naming another
	// merchant's customer: to them it doesn't exist.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "payment_intents_merchant_id_customer_id_fkey" {
		return PaymentIntent{}, apierror.NoSuch("customer", "customer", p.CustomerID)
	}
	if err != nil {
		return PaymentIntent{}, fmt.Errorf("create payment intent: %w", err)
	}
	trace.Emit(ctx, trace.LanePostgres, "payment_intent.created", trace.Fields{PaymentID: pi.ID, Data: map[string]any{
		"status": pi.Status, "amount": pi.Amount, "currency": pi.Currency, "capture_method": pi.CaptureMethod,
	}})
	return pi, nil
}

// Get returns one of the merchant's payment intents. Another merchant's
// payment intent is a 404, exactly like one that doesn't exist.
func (s *Service) Get(ctx context.Context, merchantID, id string) (PaymentIntent, error) {
	return get(ctx, s.db, merchantID, id)
}

func get(ctx context.Context, q querier, merchantID, id string) (PaymentIntent, error) {
	pi, err := scanIntent(q.QueryRow(ctx,
		`SELECT `+intentColumns+` FROM payment_intents WHERE merchant_id = $1 AND id = $2`, merchantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentIntent{}, apierror.NotFound("payment_intent", id)
	}
	if err != nil {
		return PaymentIntent{}, fmt.Errorf("get payment intent %s: %w", id, err)
	}
	return pi, nil
}

// List returns the merchant's payment intents, newest first.
func (s *Service) List(ctx context.Context, merchantID string, page Page) ([]PaymentIntent, bool, error) {
	return list(ctx, s.db, "payment_intents", "payment_intent", intentColumns, merchantID, page, scanIntent)
}

// Confirm charges the payment method: authorize, then for automatic capture,
// capture. Each processor call happens outside any database transaction
// (amendment 3.12). A processor timeout leaves the payment in processing
// and returns a 503; asking the processor again is safe because it dedupes
// on the request id.
func (s *Service) Confirm(ctx context.Context, merchantID, id, paymentMethod string) (PaymentIntent, error) {
	if paymentMethod == "" {
		return PaymentIntent{}, apierror.Missing("payment_method")
	}
	if !processor.KnownToken(paymentMethod) {
		return PaymentIntent{}, apierror.NoSuch("payment_method", "payment_method", paymentMethod)
	}
	pi, err := s.Get(ctx, merchantID, id)
	if err != nil {
		return PaymentIntent{}, err
	}
	if pi, err = s.apply(ctx, s.db, pi, EventConfirm, change{paymentMethod: &paymentMethod}); err != nil {
		return PaymentIntent{}, err
	}

	var auth processor.Authorization
	err = s.call(ctx, pi, "authorize", func(ctx context.Context, requestID string) (err error) {
		auth, err = s.proc.Authorize(ctx, processor.AuthorizeRequest{
			RequestID: requestID, PaymentMethod: paymentMethod, Amount: pi.Amount, Currency: pi.Currency,
		})
		return err
	})
	if err != nil {
		return PaymentIntent{}, err
	}

	if !auth.Approved {
		if _, err := s.apply(ctx, s.db, pi, EventDeclined, change{failureCode: &auth.DeclineCode}); err != nil {
			return PaymentIntent{}, err
		}
		return PaymentIntent{}, declined(auth.DeclineCode)
	}
	if pi.CaptureMethod == CaptureManual {
		return s.apply(ctx, s.db, pi, EventAuthorizedManual, change{authorizationID: &auth.ID})
	}
	if err := s.capture(ctx, pi, auth.ID, pi.Amount); err != nil {
		return PaymentIntent{}, err
	}
	return s.apply(ctx, s.db, pi, EventAuthorized, change{authorizationID: &auth.ID, amountCaptured: &pi.Amount})
}

// Capture collects up to the authorized amount of a manual payment; the
// rest of the authorization is released (FR-5). A nil amount captures all.
func (s *Service) Capture(ctx context.Context, merchantID, id string, amount *int64) (PaymentIntent, error) {
	pi, err := s.Get(ctx, merchantID, id)
	if err != nil {
		return PaymentIntent{}, err
	}
	if _, err := Next(pi.Status, EventCapture); err != nil {
		return PaymentIntent{}, err
	}
	toCapture := pi.Amount
	if amount != nil {
		toCapture = *amount
	}
	switch {
	case toCapture < 1:
		return PaymentIntent{}, apierror.InvalidRequest("amount_invalid", "amount_to_capture",
			"amount_to_capture must be a positive integer.")
	case toCapture > pi.Amount:
		return PaymentIntent{}, apierror.InvalidRequest("amount_too_large", "amount_to_capture",
			fmt.Sprintf("amount_to_capture can be at most the authorized amount, %d.", pi.Amount))
	}
	authID, err := authorizationOf(pi)
	if err != nil {
		return PaymentIntent{}, err
	}
	if err := s.capture(ctx, pi, authID, toCapture); err != nil {
		return PaymentIntent{}, err
	}
	return s.apply(ctx, s.db, pi, EventCapture, change{amountCaptured: &toCapture})
}

// Cancel ends a payment before any money moves. From requires_capture the
// authorization is voided at the processor first, which also settles a race
// with a capture: the processor lets only one of them through.
func (s *Service) Cancel(ctx context.Context, merchantID, id string) (PaymentIntent, error) {
	pi, err := s.Get(ctx, merchantID, id)
	if err != nil {
		return PaymentIntent{}, err
	}
	if _, err := Next(pi.Status, EventCancel); err != nil {
		return PaymentIntent{}, err
	}
	if pi.Status == StatusRequiresCapture {
		authID, err := authorizationOf(pi)
		if err != nil {
			return PaymentIntent{}, err
		}
		err = s.call(ctx, pi, "void", func(ctx context.Context, requestID string) error {
			return s.proc.Void(ctx, processor.VoidRequest{RequestID: requestID, AuthorizationID: authID})
		})
		if err != nil {
			return PaymentIntent{}, err
		}
	}
	return s.apply(ctx, s.db, pi, EventCancel, change{})
}

func (s *Service) capture(ctx context.Context, pi PaymentIntent, authID string, amount int64) error {
	return s.call(ctx, pi, "capture", func(ctx context.Context, requestID string) error {
		return s.proc.Capture(ctx, processor.CaptureRequest{RequestID: requestID, AuthorizationID: authID, Amount: amount})
	})
}

// call runs one processor operation under the processor deadline and turns
// its failure into what the client sees. The request id is fixed per payment
// and operation (amendment C3), so any repeat of this call is deduped.
func (s *Service) call(ctx context.Context, pi PaymentIntent, operation string, op func(ctx context.Context, requestID string) error) error {
	requestID := pi.ID + ":" + operation
	trace.Emit(ctx, trace.LaneAPI, "processor.call", trace.Fields{PaymentID: pi.ID, Data: map[string]any{
		"operation": operation, "request_id": requestID,
	}})
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.ProcessorTimeout)
	defer cancel()
	err := op(callCtx, requestID)

	var rejected *processor.RejectedError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, processor.ErrUnavailable):
		trace.Emit(ctx, trace.LaneAPI, "processor.unavailable", trace.Fields{PaymentID: pi.ID, Data: map[string]any{
			"operation": operation, "request_id": requestID, "status": pi.Status,
		}})
		return apierror.ProcessorUnavailable()
	case errors.As(err, &rejected):
		return unexpectedState(fmt.Sprintf("The card processor refused to %s this PaymentIntent: %s.", operation, rejected.Reason))
	case errors.Is(err, processor.ErrRequestIDReused):
		return apierror.Conflict("processor_request_mismatch", fmt.Sprintf(
			"An earlier %s of this PaymentIntent with different parameters may already have reached the card processor. Retry with the original parameters.",
			operation))
	}
	return fmt.Errorf("%s %s: %w", operation, pi.ID, err)
}

func authorizationOf(pi PaymentIntent) (string, error) {
	if pi.AuthorizationID == nil {
		return "", fmt.Errorf("payment intent %s is %s but has no authorization", pi.ID, pi.Status)
	}
	return *pi.AuthorizationID, nil
}

func declined(code string) error {
	message := "Your card was declined."
	if code == "insufficient_funds" {
		message = "Your card has insufficient funds."
	}
	return apierror.Card(code, message)
}

// change holds the columns a transition sets besides status. Nil leaves a
// column as it is.
type change struct {
	paymentMethod   *string
	authorizationID *string
	amountCaptured  *int64
	failureCode     *string
}

// How many times apply re-reads a payment whose version moved underneath it.
const applyAttempts = 3

// apply moves pi through ev. It is the only place a payment intent's status
// changes. The UPDATE matches on the version pi was read at, so of two
// requests that read the same version only one can win (optimistic locking).
//
// The loser re-reads. If the status is still the one its decision was made
// on, only a self-loop such as a refund moved the version, and it tries
// again. If the status itself changed, the facts the caller acted on are
// stale, so it refuses rather than apply a transition nobody checked.
func (s *Service) apply(ctx context.Context, q querier, pi PaymentIntent, ev Event, c change) (PaymentIntent, error) {
	for range applyAttempts {
		to, err := Next(pi.Status, ev)
		if err != nil {
			return PaymentIntent{}, err
		}
		next, err := scanIntent(q.QueryRow(ctx, `UPDATE payment_intents SET
				status = $4, version = version + 1, updated_at = now(),
				payment_method   = coalesce($5, payment_method),
				authorization_id = coalesce($6, authorization_id),
				amount_captured  = coalesce($7, amount_captured),
				failure_code     = coalesce($8, failure_code)
			WHERE merchant_id = $1 AND id = $2 AND version = $3
			RETURNING `+intentColumns,
			pi.MerchantID, pi.ID, pi.Version, to, c.paymentMethod, c.authorizationID, c.amountCaptured, c.failureCode))
		if err == nil {
			trace.Emit(ctx, trace.LanePostgres, "payment_intent.transition", trace.Fields{PaymentID: pi.ID, Data: map[string]any{
				"event": ev, "from": pi.Status, "to": next.Status, "version": next.Version,
			}})
			return next, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return PaymentIntent{}, fmt.Errorf("apply %s to %s: %w", ev, pi.ID, err)
		}
		fresh, err := get(ctx, q, pi.MerchantID, pi.ID)
		if err != nil {
			return PaymentIntent{}, err
		}
		if fresh.Status != pi.Status {
			return PaymentIntent{}, unexpectedState(fmt.Sprintf(
				"This PaymentIntent's status changed to %s while this request was in flight.", fresh.Status))
		}
		pi = fresh
	}
	return PaymentIntent{}, fmt.Errorf("apply %s to %s: version kept moving after %d attempts", ev, pi.ID, applyAttempts)
}
