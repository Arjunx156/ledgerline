package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Arjunx156/ledgerline/internal/apierror"
	"github.com/Arjunx156/ledgerline/internal/logctx"
	"github.com/Arjunx156/ledgerline/internal/payments"
)

type handlers struct {
	svc *payments.Service
}

// The JSON objects the API returns. Absent values are null, never omitted,
// so every object of a kind has the same keys.

type customerJSON struct {
	ID      string  `json:"id"`
	Object  string  `json:"object"`
	Created int64   `json:"created"`
	Email   *string `json:"email"`
	Name    *string `json:"name"`
}

func customerOut(c payments.Customer) customerJSON {
	return customerJSON{ID: c.ID, Object: "customer", Created: c.CreatedAt.Unix(), Email: c.Email, Name: c.Name}
}

type paymentIntentJSON struct {
	ID             string  `json:"id"`
	Object         string  `json:"object"`
	Amount         int64   `json:"amount"`
	AmountCaptured int64   `json:"amount_captured"`
	AmountRefunded int64   `json:"amount_refunded"`
	CaptureMethod  string  `json:"capture_method"`
	Created        int64   `json:"created"`
	Currency       string  `json:"currency"`
	Customer       *string `json:"customer"`
	FailureCode    *string `json:"failure_code"`
	PaymentMethod  *string `json:"payment_method"`
	Status         string  `json:"status"`
}

func paymentIntentOut(pi payments.PaymentIntent) paymentIntentJSON {
	return paymentIntentJSON{
		ID: pi.ID, Object: "payment_intent", Amount: pi.Amount, AmountCaptured: pi.AmountCaptured,
		AmountRefunded: pi.AmountRefunded, CaptureMethod: pi.CaptureMethod, Created: pi.CreatedAt.Unix(),
		Currency: pi.Currency, Customer: pi.CustomerID, FailureCode: pi.FailureCode,
		PaymentMethod: pi.PaymentMethod, Status: string(pi.Status),
	}
}

type listJSON[T any] struct {
	Object  string `json:"object"`
	URL     string `json:"url"`
	Data    []T    `json:"data"`
	HasMore bool   `json:"has_more"`
}

func listOut[S, T any](url string, items []S, hasMore bool, out func(S) T) listJSON[T] {
	data := make([]T, len(items))
	for i, item := range items {
		data[i] = out(item)
	}
	return listJSON[T]{Object: "list", URL: url, Data: data, HasMore: hasMore}
}

func (h handlers) createCustomer(r *http.Request) (any, error) {
	var p struct {
		Email *string `json:"email"`
		Name  *string `json:"name"`
	}
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	c, err := h.svc.CreateCustomer(r.Context(), merchantFrom(r.Context()), p.Email, p.Name)
	if err != nil {
		return nil, err
	}
	return customerOut(c), nil
}

func (h handlers) getCustomer(r *http.Request) (any, error) {
	c, err := h.svc.GetCustomer(r.Context(), merchantFrom(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		return nil, err
	}
	return customerOut(c), nil
}

func (h handlers) listCustomers(r *http.Request) (any, error) {
	p, err := page(r)
	if err != nil {
		return nil, err
	}
	items, more, err := h.svc.ListCustomers(r.Context(), merchantFrom(r.Context()), p)
	if err != nil {
		return nil, err
	}
	return listOut("/v1/customers", items, more, customerOut), nil
}

func (h handlers) createPaymentIntent(r *http.Request) (any, error) {
	var p struct {
		Amount        *int64  `json:"amount"`
		Currency      *string `json:"currency"`
		Customer      *string `json:"customer"`
		CaptureMethod *string `json:"capture_method"`
	}
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	if p.Amount == nil {
		return nil, apierror.Missing("amount")
	}
	if p.Currency == nil {
		return nil, apierror.Missing("currency")
	}
	pi, err := h.svc.Create(r.Context(), merchantFrom(r.Context()), payments.CreateParams{
		Amount: *p.Amount, Currency: *p.Currency, CustomerID: deref(p.Customer), CaptureMethod: deref(p.CaptureMethod),
	})
	if err != nil {
		return nil, err
	}
	logctx.Add(r.Context(), "payment_id", pi.ID)
	return paymentIntentOut(pi), nil
}

func (h handlers) getPaymentIntent(r *http.Request) (any, error) {
	return h.intent(r, func(id string) (payments.PaymentIntent, error) {
		return h.svc.Get(r.Context(), merchantFrom(r.Context()), id)
	})
}

func (h handlers) listPaymentIntents(r *http.Request) (any, error) {
	p, err := page(r)
	if err != nil {
		return nil, err
	}
	items, more, err := h.svc.List(r.Context(), merchantFrom(r.Context()), p)
	if err != nil {
		return nil, err
	}
	return listOut("/v1/payment_intents", items, more, paymentIntentOut), nil
}

func (h handlers) confirmPaymentIntent(r *http.Request) (any, error) {
	var p struct {
		PaymentMethod *string `json:"payment_method"`
	}
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	return h.intent(r, func(id string) (payments.PaymentIntent, error) {
		return h.svc.Confirm(r.Context(), merchantFrom(r.Context()), id, deref(p.PaymentMethod))
	})
}

func (h handlers) capturePaymentIntent(r *http.Request) (any, error) {
	var p struct {
		AmountToCapture *int64 `json:"amount_to_capture"`
	}
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	return h.intent(r, func(id string) (payments.PaymentIntent, error) {
		return h.svc.Capture(r.Context(), merchantFrom(r.Context()), id, p.AmountToCapture)
	})
}

func (h handlers) cancelPaymentIntent(r *http.Request) (any, error) {
	var p struct{}
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	return h.intent(r, func(id string) (payments.PaymentIntent, error) {
		return h.svc.Cancel(r.Context(), merchantFrom(r.Context()), id)
	})
}

// intent runs an operation on the payment intent named in the path, with
// its id on every log line.
func (h handlers) intent(r *http.Request, op func(id string) (payments.PaymentIntent, error)) (any, error) {
	id := chi.URLParam(r, "id")
	logctx.Add(r.Context(), "payment_id", id)
	pi, err := op(id)
	if err != nil {
		return nil, err
	}
	return paymentIntentOut(pi), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
