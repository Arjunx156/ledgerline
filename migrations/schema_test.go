package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/database"
	"github.com/Arjunx156/ledgerline/internal/database/dbtest"
)

// SQLSTATE codes the schema is expected to raise.
const (
	checkViolation      = "23514"
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
	restrictViolation   = "23001"
	insufficientPriv    = "42501"
)

func requirePgError(t *testing.T, err error, code, msgPart string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "expected a postgres error %s", code)
	assert.Equal(t, code, pgErr.Code, "message: %s", pgErr.Message)
	assert.Contains(t, pgErr.Message, msgPart)
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err, sql)
}

type entry struct {
	account string
	amount  int64
}

// post writes one ledger transaction and its entries in a single database
// transaction, the way the posting function will.
func post(pool *pgxpool.Pool, txnID, merchant, currency, sourceID string, entries ...entry) error {
	ctx := context.Background()
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_transactions
			(id, merchant_id, currency, description, source_type, source_id)
			VALUES ($1, $2, $3, 'test', 'capture', $4)`, txnID, merchant, currency, sourceID); err != nil {
			return err
		}
		for _, e := range entries {
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account_id, amount)
				VALUES ($1, $2, $3)`, txnID, e.account, e.amount); err != nil {
				return err
			}
		}
		return nil
	})
}

// ledgerFixture creates a merchant and its inr accounts as the app role,
// which also proves the app role has every privilege posting needs.
func ledgerFixture(t *testing.T) *dbtest.DB {
	t.Helper()
	db := dbtest.New(t)
	exec(t, db.App, `INSERT INTO merchants (id, name) VALUES ('acct_a', 'A')`)
	exec(t, db.App, `INSERT INTO ledger_accounts (id, type, currency) VALUES
		('processor_clearing:inr', 'asset', 'inr'),
		('merchant_payable:acct_a:inr', 'liability', 'inr'),
		('fee_revenue:inr', 'revenue', 'inr'),
		('processor_clearing:usd', 'asset', 'usd')`)
	return db
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n))
	return n
}

// capture of 49900 at 290 bps: fee 1447, merchant gets 48453.
var balancedCapture = []entry{
	{"processor_clearing:inr", 49900},
	{"merchant_payable:acct_a:inr", -48453},
	{"fee_revenue:inr", -1447},
}

func TestUpDownUp(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()

	require.NoError(t, database.MigrateDownAll(db.OwnerURL))
	var leftovers []string
	rows, err := db.Owner.Query(ctx, `
		SELECT 'table ' || tablename FROM pg_tables
		 WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		UNION ALL
		SELECT 'function ' || p.proname FROM pg_proc p
		  JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public'`)
	require.NoError(t, err)
	leftovers, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Empty(t, leftovers, "down migrations must remove everything")

	require.NoError(t, database.MigrateUp(db.OwnerURL))
	version, dirty, err := database.MigrationVersion(db.OwnerURL)
	require.NoError(t, err)
	assert.False(t, dirty)
	assert.Equal(t, uint(2), version)
}

func TestLedgerAcceptsBalancedTransaction(t *testing.T) {
	db := ledgerFixture(t)
	require.NoError(t, post(db.App, "txn_1", "acct_a", "inr", "pi_1", balancedCapture...))
	assert.Equal(t, 3, count(t, db.App, "ledger_entries"))
}

// The balance check is deferred to commit: each insert succeeds on its own,
// and only the commit sees the whole transaction.
func TestLedgerRejectsUnbalancedAtCommit(t *testing.T) {
	db := ledgerFixture(t)
	ctx := context.Background()
	tx, err := db.App.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO ledger_transactions (id, merchant_id, currency, description, source_type, source_id)
		VALUES ('txn_1', 'acct_a', 'inr', 'test', 'capture', 'pi_1')`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account_id, amount) VALUES
		('txn_1', 'processor_clearing:inr', 49900), ('txn_1', 'merchant_payable:acct_a:inr', -48453)`)
	require.NoError(t, err, "inserts succeed; the check waits for commit")

	requirePgError(t, tx.Commit(ctx), checkViolation, "unbalanced: entries sum to 1447")
	assert.Equal(t, 0, count(t, db.App, "ledger_transactions"), "nothing from the failed commit survives")
}

func TestLedgerRejectsMalformedTransactions(t *testing.T) {
	tests := []struct {
		name    string
		entries []entry
		code    string
		msg     string
	}{
		{"no entries", nil, checkViolation, "has 0 entries"},
		{"one entry", []entry{{"processor_clearing:inr", 100}}, checkViolation, "has 1 entries"},
		{"zero amount", []entry{{"processor_clearing:inr", 0}, {"fee_revenue:inr", 0}}, checkViolation, "ledger_entries_amount_check"},
		{"other currency", []entry{{"processor_clearing:usd", 100}, {"merchant_payable:acct_a:inr", -100}}, checkViolation, "account in another currency"},
		{"unknown account", []entry{{"nowhere:inr", 100}, {"fee_revenue:inr", -100}}, foreignKeyViolation, "ledger_entries_account_id_fkey"},
	}
	db := ledgerFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := post(db.App, "txn_bad", "acct_a", "inr", "pi_bad", tt.entries...)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			assert.Equal(t, tt.code, pgErr.Code, pgErr.Message)
			assert.Contains(t, pgErr.Message+" "+pgErr.ConstraintName, tt.msg)
		})
	}
	assert.Equal(t, 0, count(t, db.App, "ledger_transactions"))
}

func TestLedgerRejectsAppendingToCommittedTransaction(t *testing.T) {
	db := ledgerFixture(t)
	require.NoError(t, post(db.App, "txn_1", "acct_a", "inr", "pi_1", balancedCapture...))

	ctx := context.Background()
	err := pgx.BeginFunc(ctx, db.App, func(tx pgx.Tx) error {
		// balanced on its own, so only the same-transaction rule can stop it
		_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account_id, amount) VALUES
			('txn_1', 'processor_clearing:inr', 500), ('txn_1', 'fee_revenue:inr', -500)`)
		return err
	})
	requirePgError(t, err, checkViolation, "already committed")
	assert.Equal(t, 3, count(t, db.App, "ledger_entries"))
}

func TestLedgerRejectsPostingSourceTwice(t *testing.T) {
	db := ledgerFixture(t)
	require.NoError(t, post(db.App, "txn_1", "acct_a", "inr", "pi_1", balancedCapture...))
	err := post(db.App, "txn_2", "acct_a", "inr", "pi_1", balancedCapture...)
	requirePgError(t, err, uniqueViolation, "ledger_transactions_source_type_source_id_key")
}

// Two layers stop history being rewritten: the app role lacks the privilege,
// and even the owner, who has it, hits the trigger.
func TestLedgerIsAppendOnly(t *testing.T) {
	db := ledgerFixture(t)
	require.NoError(t, post(db.App, "txn_1", "acct_a", "inr", "pi_1", balancedCapture...))

	tests := []struct {
		name string
		pool *pgxpool.Pool
		sql  string
		code string
		msg  string
	}{
		{"app updates entry", db.App, `UPDATE ledger_entries SET amount = amount + 1`, insufficientPriv, "permission denied"},
		{"app deletes entry", db.App, `DELETE FROM ledger_entries`, insufficientPriv, "permission denied"},
		{"app updates transaction", db.App, `UPDATE ledger_transactions SET description = 'x'`, insufficientPriv, "permission denied"},
		{"app truncates", db.App, `TRUNCATE ledger_entries`, insufficientPriv, "permission denied"},
		{"owner updates entry", db.Owner, `UPDATE ledger_entries SET amount = amount + 1`, restrictViolation, "UPDATE on ledger_entries is not allowed"},
		{"owner deletes entry", db.Owner, `DELETE FROM ledger_entries`, restrictViolation, "DELETE on ledger_entries is not allowed"},
		{"owner updates transaction", db.Owner, `UPDATE ledger_transactions SET description = 'x'`, restrictViolation, "UPDATE on ledger_transactions is not allowed"},
		{"owner deletes transaction", db.Owner, `DELETE FROM ledger_transactions`, restrictViolation, "DELETE on ledger_transactions is not allowed"},
		{"owner truncates entries", db.Owner, `TRUNCATE ledger_entries`, restrictViolation, "TRUNCATE on ledger_entries is not allowed"},
		{"owner truncates transactions", db.Owner, `TRUNCATE ledger_transactions CASCADE`, restrictViolation, "is not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.pool.Exec(context.Background(), tt.sql)
			requirePgError(t, err, tt.code, tt.msg)
		})
	}
	assert.Equal(t, 3, count(t, db.Owner, "ledger_entries"))
}

func TestLedgerAccountIDsMatchTypeAndCurrency(t *testing.T) {
	db := dbtest.New(t)
	tests := []struct {
		id, typ, currency string
		ok                bool
	}{
		{"processor_clearing:inr", "asset", "inr", true},
		{"fee_revenue:inr", "revenue", "inr", true},
		{"merchant_payable:acct_a:inr", "liability", "inr", true},
		{"processor_clearing:usd", "asset", "inr", false},
		{"fee_revenue:inr", "asset", "inr", false},
		{"merchant_payable:acct_a:usd", "liability", "inr", false},
		{"merchant_payable::inr", "liability", "inr", false},
		{"cash:inr", "asset", "inr", false},
	}
	for _, tt := range tests {
		t.Run(tt.id+"/"+tt.typ+"/"+tt.currency, func(t *testing.T) {
			_, err := db.App.Exec(context.Background(),
				`INSERT INTO ledger_accounts (id, type, currency) VALUES ($1, $2, $3)
				 ON CONFLICT (id) DO NOTHING`, tt.id, tt.typ, tt.currency)
			if tt.ok {
				require.NoError(t, err)
				return
			}
			requirePgError(t, err, checkViolation, "ledger_accounts")
		})
	}
}

func TestPaymentIntentAmounts(t *testing.T) {
	db := dbtest.New(t)
	exec(t, db.App, `INSERT INTO merchants (id, name) VALUES ('acct_a', 'A')`)
	tests := []struct {
		name                       string
		amount, captured, refunded int64
		ok                         bool
	}{
		{"smallest", 1, 0, 0, true},
		{"largest", 99999999, 0, 0, true},
		{"zero", 0, 0, 0, false},
		{"negative", -100, 0, 0, false},
		{"above cap", 100000000, 0, 0, false},
		{"partial capture and refund", 49900, 30000, 10000, true},
		{"captured above amount", 49900, 50000, 0, false},
		{"refunded above captured", 49900, 30000, 30001, false},
		{"negative refund", 49900, 30000, -1, false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.App.Exec(context.Background(), `INSERT INTO payment_intents
				(id, merchant_id, amount, currency, status, amount_captured, amount_refunded)
				VALUES ($1, 'acct_a', $2, 'inr', 'succeeded', $3, $4)`,
				"pi_"+string(rune('a'+i)), tt.amount, tt.captured, tt.refunded)
			if tt.ok {
				require.NoError(t, err)
				return
			}
			requirePgError(t, err, checkViolation, "payment_intents")
		})
	}
}

func TestPaymentIntentRejectsUnknownEnums(t *testing.T) {
	db := dbtest.New(t)
	exec(t, db.App, `INSERT INTO merchants (id, name) VALUES ('acct_a', 'A')`)
	_, err := db.App.Exec(context.Background(), `INSERT INTO payment_intents (id, merchant_id, amount, currency, status, capture_method)
		VALUES ('pi_1', 'acct_a', 100, 'inr', 'requires_confirmation', 'later')`)
	requirePgError(t, err, checkViolation, "payment_intents_capture_method_check")
	_, err = db.App.Exec(context.Background(), `INSERT INTO payment_intents (id, merchant_id, amount, currency, status)
		VALUES ('pi_2', 'acct_a', 100, 'INR', 'pending')`)
	requirePgError(t, err, checkViolation, "payment_intents")
}

// Composite foreign keys keep every reference inside one merchant.
func TestCrossMerchantReferencesRejected(t *testing.T) {
	db := dbtest.New(t)
	exec(t, db.App, `INSERT INTO merchants (id, name) VALUES ('acct_a', 'A'), ('acct_b', 'B')`)
	exec(t, db.App, `INSERT INTO customers (id, merchant_id) VALUES ('cus_a', 'acct_a')`)
	exec(t, db.App, `INSERT INTO payment_intents (id, merchant_id, amount, currency, status)
		VALUES ('pi_a', 'acct_a', 100, 'inr', 'succeeded')`)
	exec(t, db.App, `INSERT INTO events (id, merchant_id, type, data)
		VALUES ('evt_a', 'acct_a', 'payment_intent.succeeded', '{}')`)
	exec(t, db.App, `INSERT INTO webhook_endpoints (id, merchant_id, url, secret_ciphertext)
		VALUES ('we_b', 'acct_b', 'https://b.example', '\x00')`)

	tests := []struct{ name, sql, constraint string }{
		{"payment for another merchant's customer",
			`INSERT INTO payment_intents (id, merchant_id, customer_id, amount, currency, status)
			 VALUES ('pi_b', 'acct_b', 'cus_a', 100, 'inr', 'requires_confirmation')`,
			"payment_intents_merchant_id_customer_id_fkey"},
		{"refund of another merchant's payment",
			`INSERT INTO refunds (id, merchant_id, payment_intent_id, amount, status)
			 VALUES ('re_b', 'acct_b', 'pi_a', 50, 'pending')`,
			"refunds_merchant_id_payment_intent_id_fkey"},
		{"delivery of another merchant's event",
			`INSERT INTO webhook_deliveries (id, merchant_id, event_id, endpoint_id)
			 VALUES ('whd_b', 'acct_b', 'evt_a', 'we_b')`,
			"webhook_deliveries_merchant_id_event_id_fkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.App.Exec(context.Background(), tt.sql)
			requirePgError(t, err, foreignKeyViolation, tt.constraint)
		})
	}
}

func TestIdempotencyKeyStates(t *testing.T) {
	db := dbtest.New(t)
	exec(t, db.App, `INSERT INTO merchants (id, name) VALUES ('acct_a', 'A')`)
	insert := func(key, point string, locked bool, response bool) error {
		_, err := db.App.Exec(context.Background(), `INSERT INTO idempotency_keys
			(merchant_id, key, request_method, request_path, request_hash, operation,
			 recovery_point, locked_at, response_code, response_body)
			VALUES ('acct_a', $1, 'POST', '/v1/payment_intents', sha256('x'), 'payment_intent.create', $2,
			        CASE WHEN $3 THEN now() END, CASE WHEN $4 THEN 200 END, CASE WHEN $4 THEN '{}'::bytea END)`,
			key, point, locked, response)
		return err
	}

	require.NoError(t, insert("k_started", "started", true, false))
	require.NoError(t, insert("k_finished", "finished", false, true))
	requirePgError(t, insert("k_no_response", "finished", false, false), checkViolation, "idempotency_keys")
	requirePgError(t, insert("k_early_response", "processor_requested", true, true), checkViolation, "idempotency_keys")
	requirePgError(t, insert("k_locked_finished", "finished", true, true), checkViolation, "idempotency_keys")
	requirePgError(t, insert("k_started", "started", true, false), uniqueViolation, "idempotency_keys_pkey")

	// keys are scoped per merchant
	exec(t, db.App, `INSERT INTO merchants (id, name) VALUES ('acct_b', 'B')`)
	_, err := db.App.Exec(context.Background(), `INSERT INTO idempotency_keys
		(merchant_id, key, request_method, request_path, request_hash, operation)
		VALUES ('acct_b', 'k_started', 'POST', '/v1/payment_intents', sha256('x'), 'payment_intent.create')`)
	require.NoError(t, err)
}

func TestWebhookDeliveryOncePerEventAndEndpoint(t *testing.T) {
	db := dbtest.New(t)
	exec(t, db.App, `INSERT INTO merchants (id, name) VALUES ('acct_a', 'A')`)
	exec(t, db.App, `INSERT INTO events (id, merchant_id, type, data) VALUES ('evt_1', 'acct_a', 'refund.created', '{}')`)
	exec(t, db.App, `INSERT INTO webhook_endpoints (id, merchant_id, url, secret_ciphertext) VALUES ('we_1', 'acct_a', 'https://a.example', '\x00')`)
	exec(t, db.App, `INSERT INTO webhook_deliveries (id, merchant_id, event_id, endpoint_id) VALUES ('whd_1', 'acct_a', 'evt_1', 'we_1')`)
	_, err := db.App.Exec(context.Background(),
		`INSERT INTO webhook_deliveries (id, merchant_id, event_id, endpoint_id) VALUES ('whd_2', 'acct_a', 'evt_1', 'we_1')`)
	requirePgError(t, err, uniqueViolation, "webhook_deliveries_event_id_endpoint_id_key")
}

func TestAppRoleHasLeastPrivilege(t *testing.T) {
	db := ledgerFixture(t)
	exec(t, db.App, `INSERT INTO payment_intents (id, merchant_id, amount, currency, status)
		VALUES ('pi_1', 'acct_a', 100, 'inr', 'succeeded')`)
	denied := []string{
		`CREATE TABLE sneaky (id int)`,
		`DELETE FROM payment_intents`,
		`UPDATE merchants SET name = 'x'`,
		`UPDATE ledger_accounts SET type = 'asset'`,
		`DELETE FROM events`,
		`DROP TABLE ledger_entries`,
	}
	for _, sql := range denied {
		t.Run(sql, func(t *testing.T) {
			_, err := db.App.Exec(context.Background(), sql)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			assert.Contains(t, []string{insufficientPriv}, pgErr.Code, pgErr.Message)
		})
	}
}
