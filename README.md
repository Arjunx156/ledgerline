# Ledgerline

A Stripe-style payments API in Go and PostgreSQL: retries that never charge twice, authorize, capture and refund, a double-entry ledger whose balance the database itself enforces, and signed webhooks delivered through a transactional outbox. A public proof console will let anyone attack those guarantees live and read the trace of what the system did.

## Status

The skeleton is in: the full schema, its database-level guards, a health check and CI. The payments API comes next. The design is [docs/spec.md](docs/spec.md), corrected and extended by [docs/spec-amendment-v1.2.md](docs/spec-amendment-v1.2.md), and the build order is [docs/plan.md](docs/plan.md).

## Quick start

Needs Go, Docker and [just](https://github.com/casey/just).

```bash
cp .env.example .env
just up
just migrate
just run
curl localhost:8080/healthz
```

`just check` runs everything CI runs: formatting, vet, staticcheck, govulncheck and the tests, which use the database from `just up`.

## What the database guarantees today

The ledger cannot be unbalanced, rewritten or quietly appended to, even by code that bypasses the application:

- a commit-time trigger rejects any ledger transaction whose entries don't sum to zero, have fewer than two lines, or mix currencies;
- entries can only be written by the database transaction that created their ledger transaction;
- updates, deletes and truncates are refused twice over: the application's database role lacks the privilege, and a trigger stops even the owner;
- composite foreign keys keep every customer, payment, refund and webhook reference inside one merchant.

Each rule has a test in [migrations/schema_test.go](migrations/schema_test.go).

---

Engineered by Arjun Krishnan.
