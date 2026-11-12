package payments

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/Arjunx156/ledgerline/internal/apierror"
	"github.com/Arjunx156/ledgerline/internal/ids"
	"github.com/Arjunx156/ledgerline/internal/trace"
)

// Customer is someone a merchant charges.
type Customer struct {
	ID         string
	MerchantID string
	Email      *string
	Name       *string
	CreatedAt  time.Time
}

const customerColumns = `id, merchant_id, email, name, created_at`

func scanCustomer(row pgx.Row) (Customer, error) {
	var c Customer
	err := row.Scan(&c.ID, &c.MerchantID, &c.Email, &c.Name, &c.CreatedAt)
	return c, err
}

const (
	maxEmail = 512
	maxName  = 256
)

// CreateCustomer makes a customer. Both fields are optional.
func (s *Service) CreateCustomer(ctx context.Context, merchantID string, email, name *string) (Customer, error) {
	if email != nil {
		if a, err := mail.ParseAddress(*email); err != nil || a.Name != "" || a.Address != *email || len(*email) > maxEmail {
			return Customer{}, apierror.InvalidRequest("email_invalid", "email",
				"Email must be a plain address such as asha@example.com, at most 512 characters.")
		}
	}
	if name != nil && utf8.RuneCountInString(*name) > maxName {
		return Customer{}, apierror.InvalidRequest("parameter_invalid", "name", "Name must be at most 256 characters.")
	}
	c, err := scanCustomer(s.db.QueryRow(ctx, `INSERT INTO customers (id, merchant_id, email, name)
		VALUES ($1, $2, $3, $4) RETURNING `+customerColumns, ids.New(ids.Customer), merchantID, email, name))
	if err != nil {
		return Customer{}, fmt.Errorf("create customer: %w", err)
	}
	trace.Emit(ctx, trace.LanePostgres, "customer.created", trace.Fields{Data: map[string]any{"customer": c.ID}})
	return c, nil
}

// GetCustomer returns one of the merchant's customers.
func (s *Service) GetCustomer(ctx context.Context, merchantID, id string) (Customer, error) {
	c, err := scanCustomer(s.db.QueryRow(ctx,
		`SELECT `+customerColumns+` FROM customers WHERE merchant_id = $1 AND id = $2`, merchantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, apierror.NotFound("customer", id)
	}
	if err != nil {
		return Customer{}, fmt.Errorf("get customer %s: %w", id, err)
	}
	return c, nil
}

// ListCustomers returns the merchant's customers, newest first.
func (s *Service) ListCustomers(ctx context.Context, merchantID string, page Page) ([]Customer, bool, error) {
	return list(ctx, s.db, "customers", "customer", customerColumns, merchantID, page, scanCustomer)
}
