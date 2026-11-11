# Ledgerline

A Stripe-style payments API in Go and PostgreSQL: retries that never charge twice, authorize, capture and refund, a double-entry ledger whose balance the database itself enforces, and signed webhooks delivered through a transactional outbox. A public proof console will let anyone attack those guarantees live and read the trace of what the system did.

## Status

Specification stage, with no API yet. The design is [docs/spec.md](docs/spec.md), corrected and extended by [docs/spec-amendment-v1.2.md](docs/spec-amendment-v1.2.md). The build order is [docs/plan.md](docs/plan.md).

## Develop

Needs Go and [just](https://github.com/casey/just).

```bash
just check
```

---

Engineered by Arjun Krishnan.
