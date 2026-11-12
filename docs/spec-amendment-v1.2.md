# Ledgerline: specification amendment v1.2

**Amends:** Ledgerline Project Specification v1.1 (`docs/spec.md`)
**Author:** Arjun Krishnan Suresh
**Date:** 12 November 2026
**Status:** Draft for review. Becomes binding once merged.

Read `spec.md` first, then this. Where the two differ, this document wins. Every change names the spec section it replaces, why it is needed, and the test that will hold it in place.

## 1. Corrections

A close reading of v1.1 found eleven defects, and building S2 turned up a twelfth (C12). Each would have surfaced as a failing test, a silent money bug or a security hole somewhere between M3 and the public demo. Fixing them on paper is cheaper.

| # | Section | Defect | Consequence if left |
|---|---|---|---|
| C1 | 9 | Fuzz invariant "no merchant balance is positive" contradicts the posting rules | Fuzz fails on correct code |
| C2 | 3.5, 5.3 | Recovery sweeps only payments in `processing` | A refund or manual capture that reached the processor can be lost from the ledger |
| C3 | 3.5, 5.1 | Refund processor request id `<payment id>:refund` is not unique | The second refund on a payment is deduped into the first |
| C4 | 3.3, 3.7 | Cancelling from `requires_capture` "releases the authorization", but the processor has no operation for it | The hold is never released at the processor |
| C5 | 3.3, 3.5 | The response to a processor timeout is undefined | Implementation guesses; replay and recovery disagree |
| C6 | 3.6, 4 | Merchants register arbitrary webhook URLs | Server-side request forgery from the worker |
| C7 | 3.6, 7 | The signing secret must be readable to sign, so it cannot be hashed; storage is unspecified | Plaintext secrets at rest |
| C8 | 2.3, 4 | FR-2 and FR-14 name endpoints section 4 omits; refunds cannot be read back; FR-7 says "accepts" a key, section 4 says "required" | OpenAPI and tests drift from the requirements |
| C9 | 3.4, 3.8 | No upper bound on amount; zero-amount ledger lines are possible | Overflow in fee maths; a valid capture rejected by the non-zero rule |
| C10 | 3.5 | "Canonical JSON" is undefined | Equivalent bodies hash differently and get a false 409 |
| C11 | 7, new 3.11 | Public sandbox data must expire, but ledger entries can never be deleted | Unbounded growth on a free database, or a weakened ledger guard |
| C12 | 3.7, 5.3 | The simulator dedupes on request id, but its memory is unspecified | An in-memory simulator forgets on a crash and is not shared by API and worker, so crash recovery would authorize twice |

### C1. The merchant balance invariant

Capture of A with fee F posts `merchant_payable −(A−F)`. A full refund of A posts `merchant_payable +A`. The balance is then **+F**: the merchant owes the platform the fee it kept. That is correct accounting, and v1.1's invariant rejects it.

**Replace** "no merchant balance is positive" with an exact equality, per merchant and currency:

```
balance(merchant_payable:<m>:<cur>) = Σ refunds(R) − Σ captures(A − F)
```

This is strictly stronger than a sign check: it fails on any missing, doubled or mis-signed posting. It is invariant I5 in section 3.

### C2. Recovery is driven by idempotency keys, not payment status

v1.1 recovers payments stuck in `processing`. Capture of a manual payment and every refund also cross the network in Phase 2, but neither leaves a payment in `processing`. If the server dies after the processor acts and before Phase 3 commits, and the client never retries, the processor has moved money the ledger never records.

**Replace** the recovery rule in 3.5 and flow 5.3 with:

- The idempotency key row stores what is needed to resume: `operation` (`payment_intent.confirm`, `payment_intent.capture`, `payment_intent.cancel`, `refund.create`), `resource_id` (the payment or refund id fixed in Phase 1), and `request_params` (the validated parameters, as JSON).
- The worker sweeps keys with `recovery_point = 'processor_requested'` whose lock is null or older than the stale threshold, and whose last update is older than the stale threshold. It claims them with `FOR UPDATE SKIP LOCKED`, takes the lock, and runs Phase 2 and Phase 3 exactly as a client retry would.
- A stale key at `started` has caused no external effect. The worker clears its lock and leaves it for the client.
- The 24-hour key cleanup deletes only keys at `finished`. An unfinished key older than 24 hours is an alert (invariant I9), never silent garbage.

Payment status `processing` is still swept, as a cross-check: a payment in `processing` with no key at `processor_requested` is invariant violation I9.

**Proven by:** the crash matrix (section 4), which covers every operation, not just confirm.

### C3. Processor request ids

A payment can have many refunds, so `<payment id>:refund` collides. **Replace** the request id scheme with:

| Operation | Request id |
|---|---|
| Authorize | `<pi id>:authorize` |
| Capture | `<pi id>:capture` (one capture per payment; FR-5 releases the rest) |
| Void | `<pi id>:void` |
| Refund | `<re id>:refund` |

The refund id is generated and committed in Phase 1 and stored as the key's `resource_id`, so every resume reuses it.

### C4. Void

**Add** a fifth processor operation, `Void`, called in Phase 2 when cancelling from `requires_capture`. Cancelling from `requires_confirmation` makes no processor call. The interface in 3.7 becomes: authorize, capture, void, refund, look up.

### C5. Processor timeout and 5xx

When the processor times out or returns a 5xx (including `pm_card_flaky`'s first call), the API:

- returns **503** with `type: api_error`, `code: processor_unavailable` and `Retry-After: 1`;
- does not store the response, clears the lock, and leaves the key at `processor_requested`;
- leaves the payment in `processing`.

A client retry with the same key resumes at Phase 2; if the client never retries, the C2 sweep finishes the request. Either way the processor's dedupe on request id prevents a second charge. Section 3.8's status list gains 503.

### C6. Webhook URLs and SSRF

The worker makes outbound requests to merchant-chosen URLs. **Add** to 3.6:

- Registration accepts only `https` URLs with a host name or public IP. Plain `http` is accepted only when `LEDGERLINE_ALLOW_INSECURE_WEBHOOKS=true` (local development and tests).
- The delivery client checks the **resolved** address at dial time and refuses loopback, private (RFC 1918, `fc00::/7`), link-local (including `169.254.169.254`), CGNAT (`100.64.0.0/10`), unspecified and multicast ranges. Checking at dial time defeats DNS rebinding. The development flag above also lifts this check.
- Redirects are not followed; a 3xx counts as a failed attempt.
- Sandbox merchants (section 3.11) can deliver only to the built-in receiver.

**Proven by:** a table test of the dial guard over each range, and an integration test that a registered URL resolving to `127.0.0.1` is never contacted.

### C7. Signing secret storage

Secrets are `whsec_` followed by 32 random bytes, hex-encoded. They are stored encrypted with AES-256-GCM under a 32-byte key from `LEDGERLINE_SECRET_KEY`, the nonce prefixed to the ciphertext. The plaintext is returned once, in the create response, and never logged. Key rotation is out of scope for v1.

### C8. Reconciled endpoint table

This table **replaces** section 4. The OpenAPI document is written from this table and nothing else.

**Merchant API** (`Authorization: Bearer sk_test_...`). Every POST requires `Idempotency-Key`; a missing key returns 400 `idempotency_key_missing`. FR-7's "accepts" is read as "requires".

| Method and path | Purpose |
|---|---|
| `POST /v1/customers` | Create a customer |
| `GET /v1/customers` | List customers |
| `GET /v1/customers/{id}` | Retrieve a customer |
| `POST /v1/payment_intents` | Create (amount, currency, customer, capture_method) |
| `GET /v1/payment_intents` | List payment intents |
| `GET /v1/payment_intents/{id}` | Retrieve a payment intent |
| `POST /v1/payment_intents/{id}/confirm` | Confirm with a payment_method token |
| `POST /v1/payment_intents/{id}/capture` | Capture (optional amount_to_capture) |
| `POST /v1/payment_intents/{id}/cancel` | Cancel before capture |
| `POST /v1/refunds` | Refund (payment_intent, optional amount) |
| `GET /v1/refunds` | List refunds (optional `payment_intent` filter) |
| `GET /v1/refunds/{id}` | Retrieve a refund |
| `POST /v1/webhook_endpoints` | Register a URL; the response holds the secret once |
| `GET /v1/webhook_endpoints` | List endpoints (secrets never returned) |
| `GET /v1/events` | List events, the polling fallback for missed webhooks |
| `GET /v1/events/{id}` | Retrieve an event |

**Operator API** (`/internal`, operator key):

| Method and path | Purpose |
|---|---|
| `GET /internal/balances` | Per-account balances, liabilities sign-flipped for display |
| `GET /internal/ledger_transactions` | List, filterable by `source_id` |
| `GET /internal/ledger_transactions/{id}` | One transaction with its entries |
| `GET /internal/webhook_deliveries?status=dead` | Dead deliveries |
| `POST /internal/webhook_deliveries/{id}/replay` | Re-queue a dead delivery |
| `GET /internal/invariants` | Run the section 3 invariants and report each result |

**Unauthenticated:** `GET /healthz` returns status and the running commit.

All responses carry `Request-Id`. Replayed responses also carry `Idempotent-Replayed: true`; the body and status are byte-for-byte the original.

### C9. Amount bounds and zero-amount lines

- `amount` is 1 to 99,999,999 minor units (₹9,99,999.99). Above that: 400 `amount_too_large`, also enforced by a check constraint. With the fee capped at 10,000 basis points, `amount × bps` stays below 10¹², far inside int64.
- `fee = (A × bps + 5000) / 10000` in integer arithmetic, which is half-up for positive A. `bps` comes from configuration, 0 to 10,000.
- A posting line whose amount is zero is omitted rather than written. A capture where F = 0 posts two lines; a capture where F = A posts two lines. Every transaction still has at least two lines.

**Proven by:** unit tests at A = 1, A = 99,999,999, bps = 0 and bps = 10,000.

### C10. Canonical request hash

The request hash is SHA-256 over `METHOD + "\n" + PATH + "\n" + canonical_body`, where the canonical body is:

- the JSON body parsed and re-serialised with object keys sorted bytewise, no insignificant whitespace, and no HTML escaping;
- numbers written as plain integers (bodies with fractions or exponents are rejected earlier with 400);
- an empty body treated as `{}`.

Defaults are **not** applied before hashing, so `{}` and `{"capture_method":"automatic"}` are different requests and the second returns 409. This matches Stripe's behaviour and is documented in the API reference.

**Proven by:** a table test that reordered keys and whitespace hash equal, and that changed values do not.

### C11. Sandbox data and the append-only ledger

The public demo creates merchants that must expire after an hour, but v1.1 forbids deleting ledger entries. Neither rule is weakened. Instead:

- `merchants` gains `sandbox boolean`; `ledger_transactions` gains `merchant_id`.
- One `SECURITY DEFINER` function, `purge_sandbox_merchant(id)`, owned by the database owner, deletes a sandbox merchant's data. It refuses non-sandbox merchants, and it deletes ledger rows only as **whole transactions**, so every balance including the shared `fee_revenue` and `processor_clearing` accounts stays balanced.
- The ledger's immutability trigger allows a DELETE only when the transaction-local setting `ledgerline.purge = 'sandbox'` is set, which only that function sets.
- The application role has no DELETE privilege on ledger tables, so it cannot use the setting directly.

**Proven by:** tests that the function refuses a real merchant, that the application role cannot delete, and that every invariant holds after a purge.

### C12. The simulator's memory must survive a crash

Flow 5.3 is safe only because the processor dedupes on request id: after a crash, the repeated Phase 2 gets the original result instead of a second charge. A simulator that keeps that memory in process forgets it on exactly the crash being recovered from, and the API and worker processes would each have their own copy. Recovery would then authorize twice, and the demo would prove the opposite of what it claims.

The simulator is the other side of the network, so it is modelled as a separate system with its own durable store:

- Its records live in Postgres in their own schema, `simulator`, written in the simulator's own short transactions, never inside a Ledgerline transaction. Nothing in Ledgerline reads them except reconciliation (I10) and tests.
- Each request id has one record holding the operation, a hash of its parameters, the outcome and how many calls it has seen. A repeat with the same parameters returns the stored outcome. A repeat with different parameters is rejected, as a real processor rejects a reused idempotency key.
- An authorization has its own lifecycle: `authorized`, then exactly one of `captured` or `voided`. Capture is capped at the authorized amount. The processor therefore refuses a capture after a void, or a void after a capture, on its own, so two racing requests can never both move money.
- `payment_intents` stores the processor's `authorization_id`, which capture and void reference.
- `pm_card_timeout` commits its approval and then outlives the caller's deadline, so the caller sees a timeout although the authorization happened. `pm_card_flaky` answers unavailable to the first call of each request id and records the real outcome on the second.
- Any failure inside the simulator surfaces to Ledgerline as unavailable (a processor 5xx), never as a decline.
- Unknown payment method tokens are rejected by the API with 400 `resource_missing` on `payment_method`.

**Proven by:** a test that a new simulator instance, standing in for a restarted process, returns the stored result for a repeated request id without a second authorization; tests for each token; and a race between capture and void where exactly one succeeds.

## 2. Additions to the technical design

### 3.9 Database roles (new)

Migrations run as the database owner. The API and worker connect as `ledgerline_app`, which owns nothing and holds only SELECT and INSERT on `ledger_transactions` and `ledger_entries`. This is the third layer of ledger protection, beneath the posting function and the commit-time trigger: an UPDATE fails on privilege before the trigger is consulted.

### 3.10 Invariants (new)

One package defines correctness, as SQL, and everything else calls it: the fuzz test, the crash matrix, the nightly job, `ledgerline verify`, `GET /internal/invariants` and the demo's live check.

| # | Invariant |
|---|---|
| I1 | Every ledger transaction's entries sum to zero |
| I2 | Per currency, all entries sum to zero |
| I3 | Per payment, capture postings equal `amount_captured` and refund postings equal `amount_refunded` |
| I4 | `amount_refunded ≤ amount_captured ≤ amount` |
| I5 | Each merchant payable balance equals Σ refunds − Σ (captures − fees) (C1) |
| I6 | Every ledger transaction has a source; no source is posted twice |
| I7 | Each transition to succeeded, failed, canceled or requires_capture has exactly one event |
| I8 | Each event has one delivery per endpoint enabled when it was written |
| I9 | Every finished key has a stored response; after a recovery pass no key is stale at `processor_requested` and no payment sits in `processing` without one |
| I10 | The simulator's record of authorizations, captures, voids and refunds matches the ledger and payment rows |

I10 brings forward the reconciliation item from the v1.1 cut list, because C2's correctness depends on it.

### 3.11 Crash points (new)

Named fault points sit at every recovery boundary: `after_claim`, `after_phase1_commit`, `before_processor`, `after_processor`, `before_phase3_commit`, `after_phase3_commit`. In normal builds they do nothing. Tests arm one to abandon the request at that point, as a crash would. The crash matrix runs every operation × every fault point × every simulator token × each recovery path (client retry, worker sweep, both racing) and checks every invariant after each run.

### 3.12 Concurrency rules (new)

- Isolation level READ COMMITTED with explicit row locks.
- One lock order everywhere: idempotency key, then payment intent, then refund, then ledger. Any path that locks out of order is a bug.
- No network call while a database transaction is open.
- Every status change goes through the state machine with an optimistic `version` check.
- Webhook deliveries are claimed with `FOR UPDATE SKIP LOCKED`.

## 3. Proof console and demo mode (new section 3.13)

Section 1.2's non-goal changes from "A dashboard UI" to "**A merchant dashboard**". In its place Ledgerline ships a public proof console: a site where a visitor runs each guarantee against a live sandbox and watches the system's own trace.

### Demo mode

`ledgerline demo` runs the API and worker in one process, for free hosting. Everything below is mounted **only** in demo mode and honoured **only** for sandbox merchants. Serve and worker modes never expose it.

| Endpoint | Purpose |
|---|---|
| `POST /demo/sessions` | Create a sandbox merchant, a test key and a built-in webhook endpoint. Expires after 1 hour. |
| `GET /demo/stream` | Server-sent events of the session's trace, fed by Postgres `LISTEN` so it covers both API and worker |
| `Ledgerline-Demo-Fault: <point>` | Request header arming one crash point (3.11) for that request |
| `POST /demo/receiver/mode` | Set the built-in receiver to succeed, fail the next N times, or always fail |
| `GET /demo/receiver/hits` | What the receiver saw: headers, body, signature check result |
| `POST /demo/ledger/tamper` | Attempt an unbalanced insert, an update or a delete, inside a transaction that is always rolled back, and return the database's error. Deferred triggers are forced with `SET CONSTRAINTS ALL IMMEDIATE`. |
| `GET /demo/traces/{id}` | A saved trace, for shareable links |

Limits: 10 sessions per IP per hour, 600 requests per session per minute, retry storms capped at 500 requests. The worker purges expired sessions with `purge_sandbox_merchant` (C11) and deletes their trace rows. In demo mode the stale threshold and backoff base are seconds rather than minutes, so recovery is watchable.

### Trace events

Each meaningful step emits one event. The shape is fixed at M2 so the console can be built against recorded traces while later milestones are in progress:

```
{ seq, session, request_id, at, lane, site, payment_id?, key?, data }
lane: client | api | postgres | processor | worker | receiver
site: stable dotted code, e.g. idem.claim.inserted, ledger.post.committed
```

Site codes are indexed to `file:line` at build time, and `/healthz` reports the commit, so the console links each step to the exact source that produced it.

## 4. Schema changes (section 7)

| Table | Change |
|---|---|
| merchants | Add `sandbox boolean not null default false` |
| payment_intents | `amount` check becomes 1 to 99,999,999; add `authorization_id` (C12) |
| simulator.requests, simulator.authorizations (new) | The simulator's durable memory (C12), in their own schema |
| refunds | `status` is `pending`, `succeeded` or `failed` |
| idempotency_keys | Add `operation`, `resource_id`, `request_params jsonb`, `updated_at`; index on `(recovery_point, updated_at)` for the sweep |
| ledger_transactions | Add `merchant_id` |
| ledger_entries | Immutability trigger permits DELETE only under C11's purge setting |
| webhook_endpoints | `secret` becomes `secret_ciphertext bytea` (C7) |
| demo_trace_events (new) | `seq bigserial`, session, request id, time, lane, site, payment id, key, data; index on `(session, seq)` |
| demo_receiver_hits (new) | session, received time, headers, body, signature valid |

The schema ships as four migrations: `0001` the core schema, `0002` roles and grants, `0003` the processor simulator (C12), `0004` the demo tables and purge function.

## 5. Process changes

- **Task runner.** `just` replaces `make`. Section 8's `make up migrate run` reads `just up migrate run`. `just check` runs everything CI runs.
- **Milestones.** The v1.1 milestones map onto the build plan (`docs/plan.md`) as M1 → S1, M2 → S2, M3 → S3, M4 → S4, M5 → S5, M6 → S8. S2b, S6, S7 and S9 are new.
- **Success criteria (1.3)** gain three rows: crash matrix (every scenario passes every invariant), mutation score on core packages (recorded, survivors justified in writing), and the console's scenes passing end to end against a local stack.
