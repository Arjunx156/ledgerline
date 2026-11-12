# Ledgerline

A Stripe-style payments API in Go and PostgreSQL, with a public proof console where visitors run each guarantee live. Good here means no double charge under any retry or crash, a ledger that always balances, and every public claim backed by a test a reader can run.

## Source of truth
- `docs/spec.md` (v1.1) plus `docs/spec-amendment-v1.2.md`. The amendment wins where they differ. Read both at the start of every session.
- `docs/plan.md` is the build order. Work one stage per session and stop at its "done when".
- Build only what the spec says. When it is ambiguous, stop and ask.

## Commands
- `just up` then `just migrate`: local Postgres 16 on port 5433 (5432 belongs to another project)
- `just check`: gofmt, vet, staticcheck, govulncheck, all tests. Needs `just up`. Must pass before any task is called done.
- `just run`: the API on :8080
- `just fmt`: format
- `just test -run TestLedger`: a subset of tests

## Layout
- `cmd/ledgerline`: the binary and its modes
- `internal/<concern>`: one package per concern, as listed in `docs/plan.md` section 6
- `migrations/`: plain SQL, every up with a matching down
- `docs/`: spec, amendment, plan, AI log
- `docs/private/`: git-ignored, local only (original documents and personal notes). Read it for context; never copy its content into tracked files.

## Rules
- Go, chi, pgx, golang-migrate, slog. No ORM, no broker, no new dependency without asking.
- Money is int64 minor units. No floating point in or near money code.
- Database tests run against real Postgres, never mocks. `dbtest.New(t)` gives each test its own migrated database.
- The app connects as `ledgerline_app`; a migration that adds a table grants on it in the same file, least privilege.
- No network call while a database transaction is open. Lock order: idempotency key, payment intent, refund, ledger.
- Stages S3, S4 and S5 are test-first: show the failing tests before writing the implementation.
- Tests run with `-race` in CI always, and locally whenever a C compiler is on PATH.
- Open each stage's PR with a description: what changed, why, what moved from the plan, how it was verified, where to look first. Arjun reviews it before merging. Arjun writes DESIGN.md; review it when asked, never write it.
- Arjun keeps `docs/ai-notes.md`. When a mistake of yours is caught, say so plainly so it can be logged.
