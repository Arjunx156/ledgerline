# Ledgerline

A Stripe-style payments API in Go and PostgreSQL, with a public proof console where visitors run each guarantee live. Good here means no double charge under any retry or crash, a ledger that always balances, and every public claim backed by a test a reader can run.

## Source of truth
- `docs/spec.md` (v1.1) plus `docs/spec-amendment-v1.2.md`. The amendment wins where they differ. Read both at the start of every session.
- `docs/plan.md` is the build order. Work one stage per session and stop at its "done when".
- Build only what the spec says. When it is ambiguous, stop and ask.

## Commands
- `just check`: gofmt, vet, staticcheck, govulncheck, all tests. Must pass before any task is called done.
- `just fmt`: format
- `just test ./internal/ledger -run TestPost`: a subset of tests

## Layout
- `cmd/ledgerline`: the binary and its modes
- `internal/<concern>`: one package per concern, as listed in `docs/plan.md` section 6
- `migrations/`: plain SQL, every up with a matching down
- `docs/`: spec, amendment, plan, AI log
- `docs/private/`: git-ignored, local only (original documents and personal notes). Read it for context; never copy its content into tracked files.

## Rules
- Go, chi, pgx, golang-migrate, slog. No ORM, no broker, no new dependency without asking.
- Money is int64 minor units. No floating point in or near money code.
- Database tests run against real Postgres, never mocks.
- No network call while a database transaction is open. Lock order: idempotency key, payment intent, refund, ledger.
- Stages S3, S4 and S5 are test-first: show the failing tests before writing the implementation.
- Tests run with `-race` in CI always, and locally whenever a C compiler is on PATH.
- Arjun writes DESIGN.md and every PR description. Review them when asked; never write them.
- Arjun keeps `docs/ai-notes.md`. When a mistake of yours is caught, say so plainly so it can be logged.
