package processor

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/ids"
	"github.com/Arjunx156/ledgerline/internal/trace"
)

// Simulator pretends to be a card network. Its memory lives in the
// simulator schema, written in its own transactions, so its dedupe survives
// a crash and is shared by every process (amendment C12).
type Simulator struct {
	db *pgxpool.Pool
	// outlive is how long pm_card_timeout keeps the caller waiting after it
	// has already approved. It must exceed the caller's deadline.
	outlive time.Duration
}

func NewSimulator(db *pgxpool.Pool, outlive time.Duration) *Simulator {
	return &Simulator{db: db, outlive: outlive}
}

// decisionTimeout bounds the simulator's own transaction. It no longer stops
// when the caller does, so this is what keeps a stuck database from holding
// it forever.
const decisionTimeout = 10 * time.Second

const (
	outcomeApproved = "approved"
	outcomeDeclined = "declined"
	outcomeRejected = "rejected"
)

// call is one request id's record after this call has been counted.
type call struct {
	outcome  string // "" until a call decides it
	reason   string
	n        int  // how many calls this request id has seen, this one included
	replayed bool // the outcome was already stored when this call arrived
}

func (s *Simulator) Authorize(ctx context.Context, req AuthorizeRequest) (Authorization, error) {
	var auth Authorization
	c, err := s.run(ctx, req.RequestID, "authorize",
		[]string{req.PaymentMethod, strconv.FormatInt(req.Amount, 10), req.Currency},
		func(ctx context.Context, tx pgx.Tx, c *call) error {
			if c.outcome == "" {
				if req.PaymentMethod == TokenFlaky && c.n == 1 {
					return nil // counted, still undecided: this call answers unavailable
				}
				c.outcome, c.reason = decideAuthorization(req.PaymentMethod)
				if c.outcome == outcomeApproved {
					if _, err := tx.Exec(ctx, `INSERT INTO simulator.authorizations
						(id, request_id, payment_method, amount, currency, state)
						VALUES ($1, $2, $3, $4, $5, 'authorized')`,
						ids.New(ids.Authorization), req.RequestID, req.PaymentMethod, req.Amount, req.Currency); err != nil {
						return err
					}
				}
				if err := decide(ctx, tx, req.RequestID, c); err != nil {
					return err
				}
			}
			if c.outcome == outcomeApproved {
				return tx.QueryRow(ctx, `SELECT id FROM simulator.authorizations WHERE request_id = $1`,
					req.RequestID).Scan(&auth.ID)
			}
			return nil
		})
	if err != nil {
		return Authorization{}, err
	}
	switch c.outcome {
	case "":
		return Authorization{}, ErrUnavailable
	case outcomeRejected:
		return Authorization{}, &RejectedError{Reason: c.reason}
	}
	auth.Approved = c.outcome == outcomeApproved
	auth.DeclineCode = c.reason

	// The network ate the response: the approval is recorded, but the
	// caller only ever sees its own deadline pass. A retry finds it.
	if req.PaymentMethod == TokenTimeout && c.n == 1 {
		select {
		case <-time.After(s.outlive):
		case <-ctx.Done():
			return Authorization{}, fmt.Errorf("%w: %w", ErrUnavailable, ctx.Err())
		}
	}
	return auth, nil
}

func (s *Simulator) Capture(ctx context.Context, req CaptureRequest) error {
	return s.settle(ctx, "capture", req.RequestID, req.AuthorizationID,
		[]string{req.AuthorizationID, strconv.FormatInt(req.Amount, 10)},
		func(ctx context.Context, tx pgx.Tx, authorized int64) (string, error) {
			if req.Amount < 1 || req.Amount > authorized {
				return fmt.Sprintf("capture amount %d is outside 1..%d", req.Amount, authorized), nil
			}
			_, err := tx.Exec(ctx, `UPDATE simulator.authorizations
				SET state = 'captured', amount_captured = $2, updated_at = now() WHERE id = $1`,
				req.AuthorizationID, req.Amount)
			return "", err
		})
}

func (s *Simulator) Void(ctx context.Context, req VoidRequest) error {
	return s.settle(ctx, "void", req.RequestID, req.AuthorizationID,
		[]string{req.AuthorizationID},
		func(ctx context.Context, tx pgx.Tx, _ int64) (string, error) {
			_, err := tx.Exec(ctx, `UPDATE simulator.authorizations
				SET state = 'voided', updated_at = now() WHERE id = $1`, req.AuthorizationID)
			return "", err
		})
}

// settle runs capture or void against an authorization that must still be
// open. The row lock means a capture and a void racing on one authorization
// are decided one after the other, and the loser is rejected.
func (s *Simulator) settle(ctx context.Context, operation, requestID, authID string, params []string,
	apply func(ctx context.Context, tx pgx.Tx, authorized int64) (rejection string, err error)) error {
	c, err := s.run(ctx, requestID, operation, params, func(ctx context.Context, tx pgx.Tx, c *call) error {
		if c.outcome != "" {
			return nil
		}
		var pm, state string
		var amount int64
		err := tx.QueryRow(ctx, `SELECT payment_method, state, amount FROM simulator.authorizations
			WHERE id = $1 FOR UPDATE`, authID).Scan(&pm, &state, &amount)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			c.outcome, c.reason = outcomeRejected, "no such authorization"
		case err != nil:
			return err
		case pm == TokenFlaky && c.n == 1:
			return nil
		case state != "authorized":
			c.outcome, c.reason = outcomeRejected, "authorization is already "+state
		default:
			rejection, err := apply(ctx, tx, amount)
			if err != nil {
				return err
			}
			c.outcome, c.reason = outcomeApproved, ""
			if rejection != "" {
				c.outcome, c.reason = outcomeRejected, rejection
			}
		}
		return decide(ctx, tx, requestID, c)
	})
	if err != nil {
		return err
	}
	switch c.outcome {
	case "":
		return ErrUnavailable
	case outcomeRejected:
		return &RejectedError{Reason: c.reason}
	}
	return nil
}

// run claims the request id, counting this call, lets decide fill in an
// outcome if none is stored yet, and commits. Anything that goes wrong on
// the processor's side reaches the caller as ErrUnavailable, like a 5xx.
//
// A processor that has received a request finishes it whether or not the
// caller is still listening, so the transaction runs on a context detached
// from the caller's and bounded only by decisionTimeout (amendment C12). A
// caller that has gone by then hears unavailable, as if the answer were lost.
func (s *Simulator) run(ctx context.Context, requestID, operation string, params []string,
	decide func(context.Context, pgx.Tx, *call) error) (call, error) {
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), decisionTimeout)
	defer cancel()
	var c call
	hash := paramsHash(operation, params)
	err := pgx.BeginFunc(txCtx, s.db, func(tx pgx.Tx) error {
		var storedOp string
		var storedHash []byte
		err := tx.QueryRow(txCtx, `INSERT INTO simulator.requests (request_id, operation, params_hash)
			VALUES ($1, $2, $3)
			ON CONFLICT (request_id) DO UPDATE
			   SET calls = simulator.requests.calls + 1, updated_at = now()
			RETURNING operation, params_hash, coalesce(outcome, ''), coalesce(reason, ''), calls`,
			requestID, operation, hash).Scan(&storedOp, &storedHash, &c.outcome, &c.reason, &c.n)
		if err != nil {
			return err
		}
		if storedOp != operation || string(storedHash) != string(hash) {
			return ErrRequestIDReused
		}
		c.replayed = c.outcome != ""
		return decide(txCtx, tx, &c)
	})
	switch {
	case errors.Is(err, ErrRequestIDReused):
		return call{}, err
	case err != nil:
		s.emit(ctx, operation, requestID, call{})
		return call{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	s.emit(ctx, operation, requestID, c)
	if err := ctx.Err(); err != nil {
		return call{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return c, nil
}

func decide(ctx context.Context, tx pgx.Tx, requestID string, c *call) error {
	_, err := tx.Exec(ctx, `UPDATE simulator.requests SET outcome = $2, reason = nullif($3::text, ''), updated_at = now()
		WHERE request_id = $1`, requestID, c.outcome, c.reason)
	return err
}

func decideAuthorization(pm string) (outcome, reason string) {
	switch pm {
	case TokenOK, TokenTimeout, TokenFlaky:
		return outcomeApproved, ""
	case TokenDeclined:
		return outcomeDeclined, "card_declined"
	case TokenInsufficientFunds:
		return outcomeDeclined, "insufficient_funds"
	}
	return outcomeRejected, "unknown payment method"
}

func paramsHash(operation string, params []string) []byte {
	sum := sha256.Sum256([]byte(operation + "\x00" + strings.Join(params, "\x00")))
	return sum[:]
}

// emit traces what the processor did with one call. It runs when the
// simulator's transaction has committed, so a pm_card_timeout approval shows
// here even though the caller never hears of it.
func (s *Simulator) emit(ctx context.Context, operation, requestID string, c call) {
	outcome := cmp.Or(c.outcome, "unavailable")
	data := map[string]any{"operation": operation, "request_id": requestID, "outcome": outcome, "replayed": c.replayed}
	if c.n > 0 {
		data["call"] = c.n
	}
	if c.reason != "" {
		data["reason"] = c.reason
	}
	trace.Emit(ctx, trace.LaneProcessor, "processor.handled", trace.Fields{Data: data})
}
