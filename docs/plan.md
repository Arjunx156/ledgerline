# Ledgerline: the build plan

## 1. Context and thesis

Ledgerline is specified as a Stripe-style payments API in Go and Postgres: idempotency keys, authorize/capture/refund, a double-entry ledger, signed webhooks through a transactional outbox, crash recovery. The spec has no UI. The decision taken: keep the API exactly as specified and add a public **proof console**, where a stranger runs each guarantee against a live sandbox. No deadline; quality decides, and every stage ends in something shippable.

**Thesis: every claim is executable.** Nothing on the site or in the README is asserted. Each guarantee has three proofs a visitor can reach in one click:

1. **Run it**: a console scene that performs the attack live (100 concurrent retries, a crash after the card was charged, a tampered ledger row).
2. **Read it**: every step in the live trace links to the exact source line that produced it, pinned to the deployed commit.
3. **Check it**: an evidence page fed by CI, not typed by hand: last night's fuzz input count, crash scenarios swept, invariant violations found.

That is what separates this from the many payment-API and dashboard clones: the design document is in Arjun's own words, the AI log records every mistake caught in review, and the proofs run in front of the reader.

Source of truth: `docs/spec.md` (v1.1) amended by `docs/spec-amendment-v1.2.md`. The amendment wins where they differ.

## 2. Spec corrections

A close review of v1.1 found eleven defects: a fuzz invariant that contradicts the posting rules, recovery that misses refunds and manual captures, colliding refund request ids, no void operation, an undefined timeout response, SSRF through webhook URLs, unspecified secret storage, endpoint gaps, amount overflow and zero-amount lines, an undefined canonical hash, and sandbox cleanup against an append-only ledger. All eleven are fixed in `docs/spec-amendment-v1.2.md` (C1 to C11), and each becomes an entry in DESIGN.md.

The amendment also adds the invariants, crash points, concurrency rules and the console's demo mode, and rewords non-goal 1.2 from "no dashboard UI" to "no merchant dashboard".

## 3. Architecture

```
Browser (Astro site + console islands)
   |  HTTPS/2: REST with sk_test key, SSE trace stream
   v
ledgerline serve ── Processor interface ── simulator (dedupes on request id)
   |        \
   |         trace.Recorder ── INSERT trace_events + NOTIFY
   v
PostgreSQL 16 (single source of truth; app connects as a restricted role)
   ^
   |  SKIP LOCKED claims, LISTEN
ledgerline worker ── deliveries (SSRF-guarded client) ── merchant endpoint / built-in receiver
                 └── recovery sweep, key cleanup, sandbox TTL, invariant check
```

One binary: `serve`, `worker`, `seed`, `verify`, and `demo` (serve and worker in one process, for free hosting only; the two-process path stays the tested default).

### Cross-cutting components designed in from the start

These are cheap on day one and painful to retrofit.

- **`clock.Clock`**: injected everywhere time is read. Tests and the demo run recovery and backoff in seconds.
- **`faults.Point(ctx, name)`**: named crash points at every recovery boundary (`after_claim`, `after_phase1`, `before_processor`, `after_processor`, `before_phase3_commit`, `after_commit_before_response`). No-op in production builds; the crash matrix and console scene 3 both drive it.
- **`trace.Emit(ctx, site, fields)`**: one call at each meaningful step, with a stable site code such as `idem.claim.inserted` or `ledger.post.committed`. No-op unless the merchant is traced. A `go generate` step indexes every site code to `file:line`, which is how the console links a trace step to source.
- **`invariants.Check(ctx, db)`**: the single definition of "correct", written as SQL and used by the fuzz test, the crash matrix, the nightly job, `ledgerline verify`, the operator endpoint and the console's live strip.

### The trace event is the contract between backend and console

Frozen in S2 so the console can be built against recorded fixtures while S3 to S5 are still in progress.

```
{ seq, session, request_id, at, lane, site, payment_id?, key?, data }
lane: client | api | postgres | processor | worker | receiver
```

### Invariants (the list `invariants.Check` enforces)

1. Every ledger transaction's entries sum to zero.
2. All entries in a currency sum to zero.
3. For each payment, capture postings equal `amount_captured`; refund postings equal `amount_refunded`.
4. `amount_refunded ≤ amount_captured ≤ amount`.
5. Merchant payable equals the derived formula in issue 1.
6. No ledger transaction without a source; no source posted twice.
7. Every succeeded, failed, canceled or requires_capture transition has exactly one event.
8. Every event has one delivery per endpoint enabled when it was written.
9. Every finished idempotency key has a stored response; no key is locked past the stale threshold after a recovery pass.
10. The processor's record of charges and refunds matches the ledger (the reconciliation item from the spec's cut list, promoted to core because issue 2 depends on it).

### Three layers on the ledger

Code guard (posting function rejects imbalance), database guard (deferred constraint trigger at commit; triggers reject UPDATE, DELETE and TRUNCATE on entries), and privilege guard (the app's database role holds only INSERT and SELECT on ledger tables; migrations run as the owner). The third layer is beyond the spec and costs one migration.

### Concurrency rules written down once (amendment 3.12)

Isolation is READ COMMITTED with explicit row locks. Lock order is fixed: idempotency key, then payment, then refund, then ledger. A doc comment states it and a concurrent capture-versus-refund test fails on deadlock. Optimistic `version` guards every status change. Deliveries are claimed with `FOR UPDATE SKIP LOCKED`.

### API details that signal care

`Idempotent-Replayed: true` on replays, `Request-Id` on every response and log line, keyset pagination on `(created_at, id)`, `GET /v1/events` as a polling fallback for missed webhooks, token-bucket rate limit per key returning 429 with `Retry-After`, body size and header timeouts set explicitly.

## 4. Testing: the part reviewers actually read

| Layer | What | Number it produces |
|---|---|---|
| Unit | State machine table (every legal and illegal transition), fee rounding and overflow boundary, canonical hash, signature verify, backoff schedule | Coverage on core packages |
| Integration, real Postgres | Replay byte-for-byte, key reuse, 5xx not stored, per-merchant scoping, trigger rejects imbalance and edits, restricted role cannot UPDATE, pagination with concurrent inserts, flaky receiver, dead letter and replay, SSRF guard | |
| Concurrency | 100 goroutines, one key; two over-limit refunds at once; capture versus refund | 10 consecutive passes under `-race` |
| **Crash matrix** | Exhaustive, not random: every operation × every crash point × every simulator behaviour × recovery by client retry, by worker, and by both racing. Invariants checked after each. | Scenario count (about 500) with zero violations |
| **Model-based fuzz** | Native Go fuzzing decodes bytes into a command sequence, runs it against the real system and against a 100-line in-memory reference model, and compares every response and final balance. | Inputs tried; divergences found and fixed |
| Contract | Every handler response in the test suite is validated against `openapi.yaml`. The docs cannot lie. | |
| **Mutation** | Mutation run on `ledger`, `idempotency`, `payments`: "flip a comparison, does a test fail?" made automatic. | Mutation score |
| End to end | Playwright drives all six console scenes against Docker Compose | |
| Load | k6, 20 virtual users, 30 s; then once with a profile to explain the p99 | p50, p99, throughput, machine specs |

M3, M4 and M5 are test-first: failing tests are seen and committed before the implementation.

**CI on every PR**: gitleaks, vet, staticcheck, govulncheck, migration up-down-up, tests with `-race`, coverage gate at 80% on core packages, 30 s fuzz smoke, contract check, biome, `tsc`, site build, Playwright, Lighthouse budget.

**Nightly**: 10-minute fuzz, full crash matrix, mutation run. Results are written to `proof.json` and published; the site's evidence page and landing numbers read that file. A red night shows red on the site.

## 5. The proof console

### Structure

The console is a client-side reducer over the trace stream: events in, world state out. Two consequences worth the design effort:

- **Time scrubbing is free.** Drag the timeline back and the swimlanes, state machine, key row and T-accounts all show that instant.
- **Recordings are first-class.** A trace is a JSON file. The landing hero plays a real recorded run instantly with no backend, any run gets a shareable permalink, and a sleeping sandbox never means a dead page.

Layout: left, what the merchant sees (requests, responses, the curl, Go and TypeScript for each action). Right, what the system did: six live swimlanes (client, API, Postgres, processor, worker, receiver) drawing themselves as events arrive. Below, the state machine, the idempotency key row, the T-accounts and the invariant strip ("10 of 10 holding"). Clicking any step opens a plain-English explanation and the source line on GitHub.

### Scenes

| Scene | The visitor does | They see | Proven by |
|---|---|---|---|
| 1. One payment | Create and confirm with a chosen test card | Three phases, each commit, the processor call outside any transaction, three entries posting, the signed webhook | Worked example, spec section 4 |
| 2. Retry storm | Fire 100 to 500 concurrent confirms with one key | A grid of cells marked executed, in progress, replayed (letter and colour); the client backing off; all bodies hashing identical; counters: 1 authorize, 1 capture, 1 ledger transaction | 100-goroutine test |
| 3. Crash and recover | Pick a crash point; optionally retry as the client | The key frozen at its recovery point, the stale-lock countdown, worker or client resuming with the same request id, the processor returning the original result | Crash matrix |
| 4. Break the ledger | Try an unbalanced insert, an edit, a delete | The database's own rejection text for each layer; the running sum pinned at zero | Trigger and role tests |
| 5. Webhook lab | Make the receiver fail k times; edit a delivered body | Backoff timeline with jitter, dead letter, replay; in-browser HMAC verify failing on the tampered body and on a stale timestamp | Flaky receiver, bad signature |
| 6. Capture and refund | Capture 30000 of 49900, refund 10000, then 25000, then race two refunds | Released remainder, reversal posting, `refund_exceeds_captured`, one of the racing refunds losing | Flow 5.4, concurrent refund test |

### Sandbox backend (all refused outside `demo` mode and for non-sandbox merchants)

`POST /demo/sessions` (throwaway merchant, key, built-in endpoint, 1 h TTL, per-IP limit), `GET /demo/stream` (SSE fed by Postgres LISTEN, so it spans both processes with no broker), the `Ledgerline-Demo-Fault` header, `/demo/receiver/{session}` with a failure mode, `POST /demo/ledger/tamper` (runs the attack in a transaction that always rolls back and returns the database error), trace persistence for permalinks.

### Other pages

Landing (one sentence, the recorded run, guarantees, numbers from `proof.json`), API reference generated from `openapi.yaml`, Evidence (guarantee → test → last run → count), Design (DESIGN.md), How it never charges twice (a long-form walkthrough using the same swimlane component), AI log (`docs/ai-notes.md`, unedited).

### Design direction

Decided properly in S7 through the atelier skill; the brief it starts from: a ledger book crossed with an instrument panel. Paper and ink, ruled hairlines, tabular numerals, one accent, two typefaces (a text face and a mono), motion only where it carries information (an entry posting, a lock releasing). Light and dark. Reduced-motion and a table view of the trace for screen readers; no state shown by colour alone. Essentials check before the link is shared.

## 6. Stack and layout

Backend per spec: Go (current stable), chi, pgx, golang-migrate, slog, testify, native fuzzing, k6. Added for tests only: an OpenAPI validator and a mutation tool. Web: Astro with React islands, TypeScript strict, bun, biome, types generated from `openapi.yaml`. `justfile` replaces the Makefile (standing orders; `argent ship` runs `just check`).

```
cmd/ledgerline/         serve | worker | demo | seed | verify
internal/api            router, middleware, errors, pagination, rate limit
internal/payments       state machine, intents, refunds
internal/idempotency    claim, phases, recovery points, sweep
internal/ledger         posting, fees, balances
internal/webhooks       outbox, signer, delivery, SSRF guard
internal/processor      interface, simulator
internal/invariants     the ten checks
internal/trace  internal/faults  internal/clock  internal/demo
internal/model          reference model for the fuzz test
migrations/             0001 schema, 0002 roles, 0003 demo
client/                 Go client
web/packages/client     TypeScript client (the console uses it)
web/                    site and console
openapi.yaml  justfile  Dockerfile  loadtest/
docs/                   spec, guide, DESIGN.md, adr/, ai-notes.md, threat-model.md, queries.md
```

Hosting at zero cost: site on Vercel; API in `demo` mode on a free container host with free managed Postgres (Neon first candidate; confirm tiers at S2b). Distroless image, migrations on deploy, config from environment, `.env.example` committed.

## 7. Build order

One fresh session per stage, one PR per stage, the review checklist on every PR. Stages marked ∥ can run alongside the stage above because the trace contract decouples them.

| Stage | Work | Done when |
|---|---|---|
| **S0 Foundations** | Repo scaffold; docs exported to Markdown; amendment v1.2 (section 2 fixes plus console); CLAUDE.md; public repo, branch protection | `just check` green on an empty test |
| **S1 Skeleton** (M1) | Compose, schema migration, restricted role, `/healthz`, clean shutdown, CI with Postgres | up-down-up works; CI goes red on a deliberately broken test; restricted role cannot UPDATE an entry |
| **S2 Payments** (M2) | State machine, simulator, auth, customers, intents, errors, pagination; `clock` and `trace` packages, trace contract frozen; first recorded fixtures | Table test covers every row; fixtures replay |
| **S2b Walking skeleton** | Deploy `demo` mode; a bare page that makes one payment and prints the SSE stream | Works from a phone. Flushes out hosting, CORS, SSE and HTTP/2 before anything depends on them |
| **S3 Idempotency** (M3) | Tests first; `faults` crash points, phases, recovery points, `request_params`, replay header, canonical hash | 100-goroutine test passes 10 runs in a row under `-race` |
| **S4 Ledger** (M4) | Posting, fees, refunds, triggers, `invariants`, reference model, fuzz, balances | Fuzz runs 60 s with zero divergence; hand-worked capture matches the database |
| **S5 Webhooks and recovery** (M5) | Outbox, signer, encrypted secrets, SSRF guard, backoff, dead letters, replay, key sweep, crash matrix | Matrix green; killing the worker mid-run loses and duplicates nothing |
| **S6 Hardening** | Contract tests, mutation run, rate limit, events endpoint, threat model, `queries.md` with EXPLAIN on the hot queries, nightly job and `proof.json` | Mutation survivors each killed or justified in writing |
| **S7 Console** ∥ from S3 | Design direction; reducer and swimlanes against fixtures; scenes 1, 2, 3; then 4, 5, 6; sandbox endpoints | Playwright through all six scenes; scrubbing and permalinks work |
| **S8 Site and ship** (M6) | Landing, reference, evidence, design, AI log; README; Go and TypeScript clients; k6 numbers; essentials check | A friend clones and pays in 15 minutes from the README; live link holds up on a phone |
| **S9 Depth** (open-ended) | Tamper-evident hash chain over ledger transactions, verifiable in the browser, with its throughput cost measured before and after; signing-secret rotation; lease column in place of holding a lock during delivery (the spec's own "next step"), with the comparison written up | Each is its own PR with a measured trade-off |

Presentable after S5 as an API, strong after S8, and S9 turns "at Stripe scale I would change" from a paragraph into two changes actually made and measured.

## 8. What Arjun owns

Per stage: reviews and edits the PR description before merging; answers that milestone's "must be able to explain" questions unaided before merging; adds to `docs/ai-notes.md` whenever the AI is wrong. Overall: writes DESIGN.md and one short ADR per decision in section 2; works one capture by hand; runs the fuzz, the crash matrix and k6 personally; reads every surviving mutant.

## 9. Risks

| Risk | Mitigation |
|---|---|
| Browsers cap parallel connections; the storm needs HTTP/2 | Proven at S2b. Fallback: a sandbox endpoint runs the storm server-side with the Go client and streams results. |
| Public demo endpoints | Not mounted outside `demo` mode; sandbox-scoping tested; TTL, rate limits, SSRF guard; tamper endpoint always rolls back. |
| Free-tier cold starts and sleeping database | Recorded run on the landing page; explicit wake state; permalinks served as static JSON. |
| Scope | Never cut tests, S3, S4 or the crash matrix. Cut from S9 upward, then scenes 6, 5, 4. |
| The crash matrix finds real bugs late | Crash points arrive with the first two-phase request in S3 and the matrix grows with each stage rather than arriving at the end. |

## 10. Verification

- `just check` locally and in CI: every item in the CI list in section 4.
- Spec's five-step worked example by hand with curl; one ledger transaction after the replay; `ledgerline verify` reports ten of ten.
- Concurrency test ten runs in a row; fuzz 60 s; crash matrix full; mutation report read.
- k6 at 20 virtual users for 30 s; numbers and machine specs into the README.
- Playwright through all scenes locally; then by hand on the deployed site at desktop and phone widths, screenshots reviewed, Lighthouse and essentials passed.
- Kill the deployed worker mid-delivery and confirm from the console that nothing is lost or doubled.
