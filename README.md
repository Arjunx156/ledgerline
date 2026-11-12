# Ledgerline

A Stripe-style payments API in Go and PostgreSQL: retries that never charge twice, authorize, capture and refund, a double-entry ledger whose balance the database itself enforces, and signed webhooks delivered through a transactional outbox. A public proof console will let anyone attack those guarantees live and read the trace of what the system did.

## Status

The payments API is in: customers, and payment intents you can create, confirm, capture and cancel against a simulated card processor. Idempotency keys come next, and with them retries that are safe to send. The design is [docs/spec.md](docs/spec.md), corrected and extended by [docs/spec-amendment-v1.2.md](docs/spec-amendment-v1.2.md), and the build order is [docs/plan.md](docs/plan.md).

## Quick start

Needs Go, Docker and [just](https://github.com/casey/just).

```bash
cp .env.example .env
just up
just migrate
just seed
just run
```

`just seed` prints a merchant and its secret key. The key is shown once; only its SHA-256 is stored. With the API running, make a payment:

```bash
curl localhost:8080/v1/payment_intents -H "Authorization: Bearer sk_test_..."   -d '{"amount": 49900, "currency": "inr"}'
curl localhost:8080/v1/payment_intents/pi_.../confirm -H "Authorization: Bearer sk_test_..."   -d '{"payment_method": "pm_card_ok"}'
```

Amounts are whole paise, so 49900 is ₹499.00. The simulator knows five cards: `pm_card_ok`, `pm_card_declined`, `pm_card_insufficient_funds`, `pm_card_timeout` (approves, then answers too late) and `pm_card_flaky` (fails the first call, then succeeds).

`just check` runs everything CI runs: formatting, vet, staticcheck, govulncheck and the tests, which use the database from `just up`.

## What the database guarantees today

The ledger cannot be unbalanced, rewritten or quietly appended to, even by code that bypasses the application:

- a commit-time trigger rejects any ledger transaction whose entries don't sum to zero, have fewer than two lines, or mix currencies;
- entries can only be written by the database transaction that created their ledger transaction;
- updates, deletes and truncates are refused twice over: the application's database role lacks the privilege, and a trigger stops even the owner;
- composite foreign keys keep every customer, payment, refund and webhook reference inside one merchant.

Each rule has a test in [migrations/schema_test.go](migrations/schema_test.go).

## What the API guarantees today

- Every status change goes through one function that checks the spec's state table and the row's version, so of two requests racing on one payment, only one wins. Tests race a confirm against a cancel, and a capture against a cancel, twenty times each.
- The card processor is never called inside a database transaction, and never waited on for more than five seconds. Past that the payment stays in `processing` and the client gets a 503, never a guess.
- The simulated processor keeps its memory in Postgres, so a repeated request gets its original answer even after a restart, and of a capture and a void on one authorization it lets exactly one through.
- Errors always have the same four fields and name the parameter at fault. A misspelt parameter, `49900.5` or `"49900"` as an amount is a 400, not a silent default.

---

Engineered by Arjun Krishnan.
