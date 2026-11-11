**Ledgerline — Project Specification**

PRD, TRD, flows, schema and implementation plan for a Stripe-style payments API in Go

------------------------------------------------------------------------

**Author:** Arjun Krishnan Suresh

**Version:** 1.1 — 12 November 2026 (code-free)

**Status:** Approved for build

**Amended by:** [spec-amendment-v1.2.md](spec-amendment-v1.2.md)

# 1. Overview

**Ledgerline** is a small, Stripe-style payments backend written in Go. Merchants create payments, confirm them against a simulated card processor, capture and refund them, and receive signed webhooks. Every movement of money is recorded in a double-entry ledger that is checked to always balance.

It exists to show, with running code and tests, the things a Stripe engineer cares about: safe retries, money that never goes missing, clear API design and honest written trade-offs. It is deliberately not an AI project.

## 1.1 Goals

1.  A merchant can retry any POST request with the same Idempotency-Key and never be charged twice, even with 100 concurrent retries.

2.  Every captured or refunded amount produces ledger entries that sum to zero, enforced in code **and** by the database.

3.  Merchants learn about state changes through signed webhooks that are retried until delivered or dead-lettered.

4.  Someone new to the repo can run it with 5 commands and understand the design from DESIGN.md in 10 minutes.

## 1.2 Non-goals (v1)

- Real card networks, real money, PCI scope. Cards are test tokens like pm_card_ok; no card numbers are stored.

- Multi-currency conversion. Each payment has one currency; ledger accounts are per currency.

- A dashboard UI. The product is the API; the "UI" is the README, curl examples and error messages.

- Horizontal scaling. One API process and one worker process against one Postgres is enough to demonstrate the guarantees.

## 1.3 Success criteria (measured, never estimated)

| **Criterion** | **Target** | **How it is measured** |
|----|----|----|
| Duplicate charges under concurrent retries | 0 in 100 concurrent identical requests | Concurrency test run with the race detector |
| Ledger balance | Sum of all entries = 0 after any sequence | Fuzz test, plus a DB constraint trigger |
| Webhook delivery | Exactly one success after N failures | Integration test with a flaky receiver |
| Test coverage | \> 80% on payments, ledger, idempotency | Coverage report from the test run |
| Latency (local, excluding simulated processor delay) | Record real p50/p99; no target invented in advance | k6 run, numbers written into README |

# 2. Product requirements (PRD)

## 2.1 Users

- **Merchant developer:** integrates Ledgerline into a shop backend. Wants predictable responses, safe retries and clear errors.

- **Operator (you):** runs the service, inspects the ledger and dead-lettered webhooks.

- **Reviewer (interviewer / recruiter):** reads the repo. Wants to see tests, small PRs, a clear design document.

## 2.2 User stories

| **ID** | **As a…** | **I want to…** | **So that…** |
|----|----|----|----|
| US-1 | merchant developer | create a payment for an amount and currency | I can charge a customer |
| US-2 | merchant developer | retry a request after a timeout without fear | customers are never double-charged |
| US-3 | merchant developer | authorize now and capture later | I only take money when the order ships |
| US-4 | merchant developer | refund all or part of a captured payment | I can handle returns |
| US-5 | merchant developer | receive a signed webhook when a payment changes state | my system stays in sync without polling |
| US-6 | merchant developer | verify that a webhook really came from Ledgerline | attackers cannot fake events |
| US-7 | operator | see each merchant’s balance and the entries behind it | I can explain every rupee |
| US-8 | operator | see webhooks that failed permanently | I can fix endpoints and replay |

## 2.3 Functional requirements

| **ID** | **Requirement** |
|----|----|
| FR-1 | API key auth: Authorization: Bearer sk_test\_.... Keys are stored only as SHA-256 hashes. |
| FR-2 | Customers: create, retrieve, list. |
| FR-3 | Payment intents: create, retrieve, list, confirm, capture, cancel. Amounts are positive integers in minor units (paise). |
| FR-4 | capture_method is automatic (default) or manual. Manual leaves the payment in requires_capture after authorization. |
| FR-5 | Partial capture is allowed up to the authorized amount; the remainder is released. |
| FR-6 | Refunds: full or partial, any number, total never above the captured amount. |
| FR-7 | Every mutating request accepts Idempotency-Key. Same key + same body replays the first response. Same key + different body returns 409 idempotency_key_reused. A key still being processed returns 409 idempotency_request_in_progress. |
| FR-8 | Capture, refund and their reversals post balanced ledger transactions (section 3.4). |
| FR-9 | State changes emit events (payment_intent.succeeded, .payment_failed, .requires_capture, .canceled, refund.created) and deliver them to registered webhook endpoints. |
| FR-10 | Webhook requests carry a Ledgerline-Signature header. A verification helper ships in the client library. |
| FR-11 | Failed deliveries retry with exponential backoff and jitter; after 8 attempts they become dead. |
| FR-12 | List endpoints use cursor pagination: limit (1–100, default 10) and starting_after. |
| FR-13 | Errors use one JSON shape with type, code, message and optional param. |
| FR-14 | Operator endpoints (/internal/..., separate key): balances, ledger entries per transaction, dead webhooks, replay. |

## 2.4 Non-functional requirements

- **Correctness first.** No code path may move money without a ledger transaction, and no ledger transaction may be unbalanced.

- **Durability.** Idempotency keys, events and deliveries live in Postgres, never only in memory.

- **Observability.** Structured logs with request id, merchant id, idempotency key and payment id on every line.

- **Security.** Hashed API keys, HMAC-SHA256 webhook signatures with constant-time comparison, a 5-minute signature tolerance, no secrets in logs.

- **Simplicity.** Standard library plus chi and pgx. No ORM and no message broker.

# 3. Technical design (TRD)

## 3.1 Architecture

Two processes share one Postgres database: the API process serves requests, and the worker process delivers webhooks and recovers stuck payments. Both come from one program started in two modes, serve and worker.

| **Component** | **What it does** | **Talks to** |
|----|----|----|
| Merchant (curl or a client library) | Sends API requests with a secret key and an Idempotency-Key header | API process |
| API process (serve) | Checks the key, claims the idempotency key, runs the payment logic, posts to the ledger | Processor simulator, Postgres |
| Processor simulator | Pretends to be a card network: approves, declines, times out, dedupes on request id | Called by API process and worker |
| PostgreSQL | Single source of truth: payments, refunds, idempotency keys, ledger, events, webhook deliveries | Both processes |
| Worker process (worker) | Delivers webhooks from the outbox; finishes payments stuck in processing | Postgres, merchant webhook URLs |
| Merchant webhook endpoint | Receives signed event notifications and verifies the signature | Called by the worker |

## 3.2 Technology choices

| **Area** | **Choice** | **Reason** |
|----|----|----|
| Language | Go (current stable) | One of Stripe’s main languages; strong concurrency and testing tools |
| Router | github.com/go-chi/chi/v5 | Thin layer over net/http, idiomatic middleware |
| Database driver | github.com/jackc/pgx/v5 (pgxpool) | Fast, explicit transactions, good Postgres type support |
| Migrations | golang-migrate, plain .sql files | Reviewable SQL, no ORM magic |
| Tests | testing, testify, Go fuzzing, real Postgres | Tests against the real database catch transaction bugs mocks hide |
| Lint | go vet, staticcheck | Standard, fast, CI-friendly |
| Load | k6 | Gives honest latency numbers for the README |
| CI | GitHub Actions with a Postgres service container | Every PR runs migrations and the full suite with -race |

## 3.3 Payment state machine

Status changes go through one place in the code, which checks the table below and bumps the version number (optimistic locking). Any other transition returns 400 payment_intent_unexpected_state.

| **From** | **Event** | **To** |
|----|----|----|
| (new) | create | requires_confirmation |
| requires_confirmation | confirm | processing |
| requires_confirmation | cancel | canceled |
| processing | authorized, capture_method=automatic, capture ok | succeeded |
| processing | authorized, capture_method=manual | requires_capture |
| processing | declined | failed (with failure_code) |
| processing | processor timeout | stays processing; recovery worker re-queries |
| requires_capture | capture | succeeded |
| requires_capture | cancel | canceled (authorization released) |
| succeeded | refund | succeeded (amount_refunded grows) |

## 3.4 Ledger design

Amounts are int64 minor units. Each entry has a signed amount: positive = debit, negative = credit. A ledger transaction is valid only if its entries sum to zero. Entries are append-only; mistakes are corrected with new reversing transactions.

| **Account** | **Type** | **Meaning** |
|----|----|----|
| processor_clearing:\<cur\> | Asset | Money the processor owes us for captured payments |
| merchant_payable:\<merchant\>:\<cur\> | Liability | Money we owe the merchant |
| fee_revenue:\<cur\> | Revenue | Our fee |

Posting rules, with A = amount captured, F = fee (configurable basis points, rounded half-up), R = amount refunded:

| **Business event** | **Entries** | **Check** |
|----|----|----|
| Capture of A | processor_clearing +A; merchant_payable −(A−F); fee_revenue −F | A − (A−F) − F = 0 |
| Refund of R (fee kept) | merchant_payable +R; processor_clearing −R | R − R = 0 |
| Authorization only | none (a hold, not a money movement) | — |

Two layers enforce balance: the ledger posting function rejects unbalanced entries before touching the database, and a database rule re-checks the sum when the transaction commits. Ledger transactions are unique per source (capture or refund id), so the same capture or refund can never be posted twice, even through a bug elsewhere.

## 3.5 Idempotency design

This follows the pattern Stripe describes publicly: the client sends a unique Idempotency-Key on each mutating request, and the server guarantees the side effect happens at most once.

Calling the processor inside a long database transaction would hold locks during network I/O. So a request runs in **atomic phases**, each one Postgres transaction, with a recovery_point saved after each phase:

1.  **Claim:** try to insert the key for this merchant; if it already exists, lock that row and then: if the request hash differs, return 409 idempotency_key_reused; if it is complete, replay the stored response; if it is locked and the lock is younger than 60 s, return 409 idempotency_request_in_progress; if the lock is older, take it over and resume from recovery_point.

2.  **Phase 1 (\`started\` → \`processor_requested\`):** move the payment to processing. Commit.

3.  **Phase 2 (network):** call the processor with request_id = \<payment id\>:\<operation\>. The simulator, like real processors, dedupes on that id, so repeating this phase is safe.

4.  **Phase 3 (\`processor_requested\` → \`finished\`):** apply the result: final status, ledger posting, event + deliveries, stored response, recovery_point = finished, lock cleared. Commit.

Rules: only 2xx and 4xx responses are stored. A 5xx clears the lock so the client can retry. Keys expire after 24 hours (a cleanup job deletes them). The request hash is SHA-256 over method, path and the canonical JSON body.

## 3.6 Webhooks (transactional outbox)

- In the same transaction as a state change, insert one row into events and one webhook_deliveries row per enabled endpoint. An event can never be lost or sent for a change that rolled back.

- The worker claims due deliveries with row locks that other workers skip, so two workers never send the same attempt.

- Request: POST \<url\> with a JSON event body, a 5-second timeout and the header Ledgerline-Signature: t=\<unix\>,v1=\<hex HMAC-SHA256(secret, "\<t\>." + body)\>.

- A 2xx response marks the delivery succeeded. Anything else schedules next_attempt_at = now + min(10s × 2^attempt, 6h) plus up to 20% jitter. After 8 attempts the delivery becomes dead.

- Delivery is at-least-once, so receivers dedupe on event.id. The README says so.

## 3.7 Processor simulator

| **Payment method token** | **Behaviour** |
|----|----|
| pm_card_ok | Authorizes; capture succeeds |
| pm_card_declined | Declines with card_declined |
| pm_card_insufficient_funds | Declines with insufficient_funds |
| pm_card_timeout | Sleeps past the client timeout, then succeeds (tests the recovery path) |
| pm_card_flaky | Fails with a 503 on the first call for each request id, then succeeds |

The simulator sits behind a processor interface with four operations: authorize, capture, refund and look up. Tests can then count calls and inject failures, and a real processor could replace it later.

## 3.8 API conventions

- IDs: prefix + 24 random letters and digits from a cryptographically secure generator (cus\_, pi\_, re\_, evt\_, txn\_).

- JSON request bodies; responses are the full object. Timestamps are Unix seconds.

- Every error has the same four fields: type (for example invalid_request_error), code (for example amount_invalid), a human-readable message, and the param it refers to.

- Status codes: 400 invalid request, 401 bad key, 402 card declined, 404 not found, 409 idempotency conflicts, 429 rate limited, 500 internal.

# 4. API reference

| **Method & path** | **Purpose** | **Idempotent?** |
|----|----|----|
| POST /v1/customers | Create a customer | Key required |
| GET /v1/customers/{id} | Retrieve | Naturally |
| POST /v1/payment_intents | Create (amount, currency, customer, capture_method) | Key required |
| GET /v1/payment_intents \| /{id} | List / retrieve | Naturally |
| POST /v1/payment_intents/{id}/confirm | Confirm with a payment_method token | Key required |
| POST /v1/payment_intents/{id}/capture | Capture (optional amount_to_capture) | Key required |
| POST /v1/payment_intents/{id}/cancel | Cancel before capture | Key required |
| POST /v1/refunds | Refund (payment_intent, optional amount) | Key required |
| POST /v1/webhook_endpoints | Register a URL; response includes the signing secret once | Key required |
| GET /internal/balances | Per-account balances | Operator key |
| GET /internal/webhook_deliveries?status=dead | Dead deliveries | Operator key |
| POST /internal/webhook_deliveries/{id}/replay | Re-queue a dead delivery | Operator key |

### Worked example: what a correct implementation returns

| **Step** | **Request** | **Expected response** |
|----|----|----|
| 1 | POST /v1/payment_intents, amount 49900, currency inr, Idempotency-Key order-1001-create | 200, a payment intent with status requires_confirmation and amount_captured 0 |
| 2 | POST /v1/payment_intents/{id}/confirm, payment_method pm_card_ok, Idempotency-Key order-1001-confirm | 200, status succeeded, amount_captured 49900 |
| 3 | Repeat step 2 exactly | 200, byte-for-byte the same response; no second charge, no second ledger transaction |
| 4 | Repeat step 2 with the same key but a different body | 409, error code idempotency_key_reused |
| 5 | Confirm a new payment with pm_card_declined | 402, error code card_declined; status failed; no ledger entries |

# 5. Application flows

## 5.1 Happy path: create → confirm (automatic capture)

1.  Client POST /v1/payment_intents with key K1. Server claims K1, inserts the payment in requires_confirmation, stores the response and returns 200.

2.  Client POST .../confirm with key K2. Server claims K2. Phase 1 moves the payment to processing and commits.

3.  Phase 2 calls Authorize then Capture on the simulator with request ids pi_x:authorize and pi_x:capture.

4.  Phase 3, in one transaction: status succeeded, amount_captured = A, ledger posting for the capture, event payment_intent.succeeded, one delivery row per endpoint, response stored, K2 finished.

5.  The worker delivers the webhook; the merchant verifies the signature and returns 200.

## 5.2 Client times out and retries

1.  Client sends confirm with K2; the network drops the response.

2.  Client retries with K2 and the same body.

3.  If the first request already finished, the server replays the stored response. Exactly one charge exists.

4.  If it is still running, the server returns 409 idempotency_request_in_progress; the client backs off and retries.

## 5.3 Server crashes mid-request

1.  The server crashes after Phase 2 (processor charged) but before Phase 3 commits.

2.  K2 is left locked at processor_requested; the payment is processing.

3.  Either the client retries after 60 s and takes over the stale lock, or the recovery worker finds payments stuck in processing for over 60 s.

4.  Either path repeats Phase 2 with the same request id. The simulator returns the original result, not a new charge. Phase 3 then completes normally.

## 5.4 Manual capture and partial refund

1.  Create with capture_method: manual, confirm, and the status becomes requires_capture. Event payment_intent.requires_capture. No ledger entries yet.

2.  Capture 30000 of 49900: status succeeded, ledger posts on 30000, the remaining 19900 is released.

3.  Refund 10000: refund row re\_..., ledger reversal posting, amount_refunded = 10000, event refund.created.

4.  Refund 25000: rejected with 400 refund_exceeds_captured, because 10000 + 25000 \> 30000.

# 6. Developer experience brief (replaces a UI/UX brief)

Ledgerline has no screens; its interface is the API and the repo. The same care goes into these:

- **README** opens with one sentence on what it is, a 5-command quickstart, an architecture diagram, curl examples and the measured numbers.

- **Error messages** say what was wrong and how to fix it, and name the parameter. No stack traces in responses.

- **Consistent names:** snake_case JSON, prefixed ids, the same field names across resources.

- **Go client library** with typed requests, automatic retries that reuse the idempotency key, and a webhook verification helper.

- **OpenAPI description** of the API so anyone can generate a client in another language.

# 7. Backend schema

One database migration creates everything below, and a matching down migration removes it.

Each table below is described by its purpose, its important columns and the rules the database itself must enforce. The migration must implement every rule in the "Must enforce" column.

| **Table** | **Purpose and key columns** | **Must enforce** |
|----|----|----|
| merchants | One row per merchant: id, name, created time | Primary key on id |
| api_keys | Secret keys stored only as a SHA-256 hash, linked to a merchant, with an operator flag | Hash is the primary key; raw keys never stored |
| customers | Merchant’s customers: id, merchant, email, name | Belongs to an existing merchant |
| payment_intents | id, merchant, customer, amount (whole paise), currency, capture method, status, payment method token, amount captured, amount refunded, failure code, version number, timestamps | Amount above zero; capture method is automatic or manual; captured never above amount; refunded never above captured; index for listing by merchant newest first; index to find payments stuck in processing |
| refunds | id, merchant, payment intent, amount, status | Amount above zero; must reference a real payment |
| idempotency_keys | merchant + key, request method, path, request hash, recovery point, lock time, stored response code and body | merchant + key is unique (the primary key) |
| ledger_accounts | id such as merchant_payable:\<merchant\>:inr, type (asset, liability, revenue), currency | Type limited to the three values |
| ledger_transactions | id, currency, description, source type (capture or refund), source id | Source type + source id unique, so nothing is posted twice |
| ledger_entries | transaction, account, signed amount (positive = debit, negative = credit) | Amount never zero; entries of one transaction sum to zero, checked at commit time; entries can never be updated or deleted |
| events | id, merchant, type (for example payment_intent.succeeded), data, created time | Written in the same transaction as the change it describes |
| webhook_endpoints | id, merchant, URL, signing secret, enabled flag | — |
| webhook_deliveries | event, endpoint, status (pending, succeeded, dead), attempt count, next attempt time, last status code, last error | One delivery per event + endpoint; index on next attempt time for pending rows |

> An account’s balance is the sum of its entries. Under the debit-positive convention a liability balance is negative, so the operator endpoint flips the sign for display.

# 8. Implementation plan

This table holds the milestones and how each one is known to be done. Amendment v1.2 maps them onto the build plan in [plan.md](plan.md).

| **Milestone** | **Days** | **Done when** |
|----|----|----|
| M0 Go basics + design | 1–2 | Go Tour done; this spec reviewed; repo created with a README stub |
| M1 Skeleton | 3 | make up migrate run serves /healthz; CI green on an empty test |
| M2 Payments API | 4–5 | Create/confirm/capture/cancel with the state machine; handler tests pass |
| M3 Idempotency | 6–7 | The 100-goroutine test passes under -race; replay and 409 cases tested |
| M4 Ledger | 8–9 | Capture and refund post balanced transactions; fuzz test and DB trigger both reject imbalance |
| M5 Webhooks + recovery | 10–11 | Flaky-receiver test passes; crash-recovery test passes |
| M6 Ship | 12–13 | README, DESIGN.md, OpenAPI, k6 numbers, coverage report, deployed demo |

# 9. Test strategy

| **Test** | **Kind** | **What it proves** |
|----|----|----|
| State machine table test | Unit | Every legal transition works; every illegal one is rejected |
| Fee rounding | Unit | Integer fee maths; no floats anywhere |
| Posting function rejects imbalance | Unit | The code-level guard |
| DB trigger rejects imbalance | Integration | The database-level guard (insert unbalanced entries directly; expect commit error) |
| Idempotent replay | Integration | Same key + body returns an identical response; one charge |
| Key reuse with another body | Integration | Returns 409 idempotency_key_reused |
| 100 concurrent confirms | Concurrency | Exactly one authorize, one capture and one ledger transaction |
| Crash between phases | Integration | A recovery run finishes the payment once (simulated by stopping after Phase 2) |
| Random payment/refund sequences | Fuzz | The global sum is 0, no merchant balance is positive, refunded ≤ captured |
| Flaky webhook receiver | Integration | Retries with backoff; exactly one success recorded |
| Bad signature | Unit | The verification helper rejects a tampered body, a wrong secret and an old timestamp |
| Pagination | Integration | No duplicates or gaps across pages while new rows are inserted |

# 10. Risks and open questions

| **Risk** | **Impact** | **Mitigation** |
|----|----|----|
| Learning Go while building | Slow first days | Days 1–2 are only for Go basics; copy patterns from this spec; small PRs |
| Idempotency phases are subtle | Hidden double-charge bug | Write the concurrency and crash tests before the implementation |
| Webhook worker holds a lock during an HTTP call | Slow endpoint blocks one worker slot | 5 s timeout; v1 accepts this, DESIGN.md names a lease column as the next step |
| Scope creep | Nothing ships | Cut list: reconciliation, Go client polish, Ruby client and k6 go first |

Open question: deploy target. Any small host with managed Postgres works; choose one with a free tier on Day 12.

