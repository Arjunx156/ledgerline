-- Core schema: spec section 7 with amendment v1.2 section 4.
-- Money is bigint minor units everywhere. Rows that must belong to the same
-- merchant are tied with composite foreign keys, so a cross-merchant reference
-- is impossible even through a bug in the application.

CREATE TABLE merchants (
    id         text PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Raw keys are never stored, only their SHA-256.
CREATE TABLE api_keys (
    key_hash    bytea PRIMARY KEY CHECK (octet_length(key_hash) = 32),
    merchant_id text NOT NULL REFERENCES merchants (id),
    operator    boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE customers (
    id          text PRIMARY KEY,
    merchant_id text NOT NULL REFERENCES merchants (id),
    email       text,
    name        text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, id)
);
CREATE INDEX customers_list ON customers (merchant_id, created_at DESC, id DESC);

CREATE TABLE payment_intents (
    id              text PRIMARY KEY,
    merchant_id     text NOT NULL REFERENCES merchants (id),
    customer_id     text,
    amount          bigint NOT NULL CHECK (amount BETWEEN 1 AND 99999999),
    currency        text NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
    capture_method  text NOT NULL DEFAULT 'automatic'
                    CHECK (capture_method IN ('automatic', 'manual')),
    status          text NOT NULL CHECK (status IN (
                        'requires_confirmation', 'processing', 'requires_capture',
                        'succeeded', 'failed', 'canceled')),
    payment_method  text,
    amount_captured bigint NOT NULL DEFAULT 0 CHECK (amount_captured >= 0),
    amount_refunded bigint NOT NULL DEFAULT 0 CHECK (amount_refunded >= 0),
    failure_code    text,
    version         integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, id),
    FOREIGN KEY (merchant_id, customer_id) REFERENCES customers (merchant_id, id),
    CHECK (amount_captured <= amount),
    CHECK (amount_refunded <= amount_captured)
);
CREATE INDEX payment_intents_list ON payment_intents (merchant_id, created_at DESC, id DESC);
CREATE INDEX payment_intents_processing ON payment_intents (updated_at) WHERE status = 'processing';

CREATE TABLE refunds (
    id                text PRIMARY KEY,
    merchant_id       text NOT NULL REFERENCES merchants (id),
    payment_intent_id text NOT NULL,
    amount            bigint NOT NULL CHECK (amount BETWEEN 1 AND 99999999),
    status            text NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (merchant_id, payment_intent_id) REFERENCES payment_intents (merchant_id, id)
);
CREATE INDEX refunds_list ON refunds (merchant_id, created_at DESC, id DESC);
CREATE INDEX refunds_payment_intent ON refunds (payment_intent_id);

-- A key is finished exactly when its response is stored, and a finished key
-- holds no lock. Spec 3.5 and amendment C2.
CREATE TABLE idempotency_keys (
    merchant_id    text NOT NULL REFERENCES merchants (id),
    key            text NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),
    request_method text NOT NULL,
    request_path   text NOT NULL,
    request_hash   bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    operation      text NOT NULL,
    resource_id    text,
    request_params jsonb,
    recovery_point text NOT NULL DEFAULT 'started'
                   CHECK (recovery_point IN ('started', 'processor_requested', 'finished')),
    locked_at      timestamptz,
    response_code  integer,
    response_body  bytea,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, key),
    CHECK ((recovery_point = 'finished') = (response_code IS NOT NULL AND response_body IS NOT NULL)),
    CHECK (recovery_point <> 'finished' OR locked_at IS NULL)
);
CREATE INDEX idempotency_keys_sweep ON idempotency_keys (recovery_point, updated_at)
    WHERE recovery_point <> 'finished';
CREATE INDEX idempotency_keys_expiry ON idempotency_keys (created_at)
    WHERE recovery_point = 'finished';

-- Account ids encode their type and currency; the check keeps the two in step.
CREATE TABLE ledger_accounts (
    id         text PRIMARY KEY,
    type       text NOT NULL CHECK (type IN ('asset', 'liability', 'revenue')),
    currency   text NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (type = 'asset' AND id = 'processor_clearing:' || currency) OR
        (type = 'revenue' AND id = 'fee_revenue:' || currency) OR
        (type = 'liability' AND id ~ ('^merchant_payable:[A-Za-z0-9_]+:' || currency || '$'))
    )
);

CREATE TABLE ledger_transactions (
    id          text PRIMARY KEY,
    merchant_id text NOT NULL REFERENCES merchants (id),
    currency    text NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
    description text NOT NULL,
    source_type text NOT NULL CHECK (source_type IN ('capture', 'refund')),
    source_id   text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_type, source_id)
);

-- Signed amounts: positive is a debit, negative a credit.
CREATE TABLE ledger_entries (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id text NOT NULL REFERENCES ledger_transactions (id),
    account_id     text NOT NULL REFERENCES ledger_accounts (id),
    amount         bigint NOT NULL CHECK (amount <> 0),
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_transaction ON ledger_entries (transaction_id);
CREATE INDEX ledger_entries_account ON ledger_entries (account_id);

-- Entries may only be written by the database transaction that created their
-- ledger transaction. Otherwise a balanced pair could be appended to history
-- months later and the commit-time check would wave it through. Fails closed
-- inside a savepoint (xmin is then the subtransaction's), so posting never uses one.
CREATE FUNCTION ledger_entry_same_transaction() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    creator bigint;
BEGIN
    SELECT xmin::text::bigint INTO creator
    FROM ledger_transactions WHERE id = NEW.transaction_id;
    IF creator IS DISTINCT FROM (pg_current_xact_id()::text::bigint % 4294967296) THEN
        RAISE EXCEPTION 'ledger transaction % is already committed; post a new transaction instead',
            NEW.transaction_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER ledger_entries_same_transaction
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entry_same_transaction();

-- The database-level balance guard. It runs at commit rather than per row
-- because a transaction is only balanced once all its entries are in.
CREATE FUNCTION ledger_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    txn_id    text;
    txn_cur   text;
    n_entries bigint;
    total     numeric;
    n_foreign bigint;
BEGIN
    -- one function for both tables; each branch only touches fields its row has
    IF TG_TABLE_NAME = 'ledger_transactions' THEN
        txn_id := NEW.id;
    ELSE
        txn_id := NEW.transaction_id;
    END IF;
    SELECT currency INTO txn_cur FROM ledger_transactions WHERE id = txn_id;
    SELECT count(*), coalesce(sum(e.amount), 0),
           count(*) FILTER (WHERE a.currency <> txn_cur)
      INTO n_entries, total, n_foreign
      FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
     WHERE e.transaction_id = txn_id;
    IF n_entries < 2 THEN
        RAISE EXCEPTION 'ledger transaction % has % entries; at least 2 are required', txn_id, n_entries
            USING ERRCODE = 'check_violation';
    END IF;
    IF total <> 0 THEN
        RAISE EXCEPTION 'ledger transaction % is unbalanced: entries sum to %', txn_id, total
            USING ERRCODE = 'check_violation';
    END IF;
    IF n_foreign > 0 THEN
        RAISE EXCEPTION 'ledger transaction % in % posts to an account in another currency', txn_id, txn_cur
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER ledger_transactions_balanced
    AFTER INSERT ON ledger_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_transaction_balanced();

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_transaction_balanced();

-- History is append-only. Mistakes are corrected with reversing transactions.
CREATE FUNCTION ledger_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: the ledger is append-only', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END $$;

CREATE TRIGGER ledger_transactions_immutable
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_immutable();
CREATE TRIGGER ledger_transactions_no_truncate
    BEFORE TRUNCATE ON ledger_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_immutable();
CREATE TRIGGER ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_immutable();
CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_immutable();

CREATE TABLE events (
    id          text PRIMARY KEY,
    merchant_id text NOT NULL REFERENCES merchants (id),
    type        text NOT NULL CHECK (type IN (
                    'payment_intent.succeeded', 'payment_intent.payment_failed',
                    'payment_intent.requires_capture', 'payment_intent.canceled',
                    'refund.created')),
    data        jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, id)
);
CREATE INDEX events_list ON events (merchant_id, created_at DESC, id DESC);

-- The secret must be readable to sign deliveries, so it is encrypted rather
-- than hashed. Amendment C7.
CREATE TABLE webhook_endpoints (
    id                text PRIMARY KEY,
    merchant_id       text NOT NULL REFERENCES merchants (id),
    url               text NOT NULL,
    secret_ciphertext bytea NOT NULL,
    enabled           boolean NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, id)
);
CREATE INDEX webhook_endpoints_list ON webhook_endpoints (merchant_id, created_at DESC, id DESC);

CREATE TABLE webhook_deliveries (
    id               text PRIMARY KEY,
    merchant_id      text NOT NULL REFERENCES merchants (id),
    event_id         text NOT NULL,
    endpoint_id      text NOT NULL,
    status           text NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'succeeded', 'dead')),
    attempt_count    integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_status_code integer,
    last_error       text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id),
    FOREIGN KEY (merchant_id, event_id) REFERENCES events (merchant_id, id),
    FOREIGN KEY (merchant_id, endpoint_id) REFERENCES webhook_endpoints (merchant_id, id)
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_dead ON webhook_deliveries (merchant_id, updated_at DESC) WHERE status = 'dead';
