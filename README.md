# Ledgerline

A Stripe-style payments API in Go and PostgreSQL: retries that never charge twice, authorize, capture and refund, a double-entry ledger whose balance the database itself enforces, and signed webhooks delivered through a transactional outbox. A public proof console will let anyone attack those guarantees live and read the trace of what the system did.

## Status

The payments API is in: customers, and payment intents you can create, confirm, capture and cancel against a simulated card processor. A demo mode now hands out sandbox sessions and streams every step the server takes for each one live, with a bare page that runs it from a phone. Idempotency keys come next, and with them retries that are safe to send. The design is [docs/spec.md](docs/spec.md), corrected and extended by [docs/spec-amendment-v1.2.md](docs/spec-amendment-v1.2.md), and the build order is [docs/plan.md](docs/plan.md).

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

## Demo mode

`ledgerline demo` is `serve` plus what the public proof console needs. Anyone can start a sandbox session: a fresh merchant with its own key, good for an hour, at most ten an hour from one network. Every step the server takes for that session is stored in Postgres and pushed to the browser as server-sent events, so a dropped connection picks up exactly where it left off.

Try it locally with `just demo` in one terminal and `just page` in another, then open <http://localhost:5173/?api=http://localhost:8080>. The page starts a session, pays it with the test card you pick and prints the trace as it arrives. "Send 20 requests at once" shows whether the connection multiplexes: over HTTP/1.1 a browser sends six at a time, over HTTP/2 all twenty go together.

## Deploying the demo

Both halves run free: Postgres on Neon, and the API and the page on Render, described by [render.yaml](render.yaml). The container only ever holds the app role's credentials. Migrations run from your own machine, as the owner.

1. **Database.** Create a Neon project on Postgres 16 in AWS Asia Pacific (Singapore), next to Render's Singapore region. In its SQL Editor, create the role the API connects as:

   ```sql
   CREATE ROLE ledgerline_app LOGIN PASSWORD 'a long random password';
   ```

   Create it with SQL, not in the Console: roles made in the Console join `neon_superuser`, which would undo the least-privilege grants the migrations make.

2. **Migrations.** Copy the owner's direct connection string from the Neon dashboard (connection pooling off), then from a checkout:

   ```bash
   LEDGERLINE_MIGRATE_URL='postgresql://neondb_owner:...@ep-....ap-southeast-1.aws.neon.tech/neondb?sslmode=require' go run ./cmd/ledgerline migrate up
   ```

   Render deploys a merge as soon as CI passes, so apply a branch's new migrations before merging it.

3. **Render.** In the Render dashboard choose New, then Blueprint, connect this repository and pick the branch. Render reads render.yaml and asks for one secret, `LEDGERLINE_DATABASE_URL`: paste `ledgerline_app`'s direct connection string, ending in `?sslmode=require`. It must not be the `-pooler` host. The trace stream holds a `LISTEN`, which needs a session of its own, and Neon's pooler takes the connection back after every transaction.

4. **Addresses.** Render names each address after its service, first come, first served. If either service didn't get the address in render.yaml (`ledgerline-api.onrender.com` and `ledgerline-skeleton.onrender.com`), update `LEDGERLINE_DEMO_ORIGINS` and the page's `Content-Security-Policy` there, and the `ledgerline-api` meta tag in [web/skeleton/index.html](web/skeleton/index.html).

5. Once the branch is merged, point the Blueprint at `main`.

Free services sleep when nobody is using them. The first request after a quiet spell takes about a minute while the API wakes, and the page says so while it waits.

---

Engineered by Arjun Krishnan.
