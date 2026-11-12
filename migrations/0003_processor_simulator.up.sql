-- The card processor's side of the network (amendment C12). It is a separate
-- system with its own durable memory, so its dedupe on request id survives
-- the crash it exists to make safe, and the API and worker share it.

ALTER TABLE payment_intents ADD COLUMN authorization_id text;

CREATE SCHEMA simulator;

-- One row per request id. outcome stays null while the processor has only
-- answered "unavailable", so the next call decides it.
CREATE TABLE simulator.requests (
    request_id  text PRIMARY KEY,
    operation   text NOT NULL CHECK (operation IN ('authorize', 'capture', 'void')),
    params_hash bytea NOT NULL CHECK (octet_length(params_hash) = 32),
    outcome     text CHECK (outcome IN ('approved', 'declined', 'rejected')),
    reason      text,
    calls       integer NOT NULL DEFAULT 1 CHECK (calls >= 1),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK ((outcome IN ('declined', 'rejected')) = (reason IS NOT NULL))
);

-- An authorization is captured or voided, never both.
CREATE TABLE simulator.authorizations (
    id              text PRIMARY KEY,
    request_id      text NOT NULL UNIQUE REFERENCES simulator.requests (request_id),
    payment_method  text NOT NULL,
    amount          bigint NOT NULL CHECK (amount > 0),
    currency        text NOT NULL,
    state           text NOT NULL CHECK (state IN ('authorized', 'captured', 'voided')),
    amount_captured bigint NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (amount_captured BETWEEN 0 AND amount),
    CHECK ((state = 'captured') = (amount_captured > 0))
);

-- UPDATE only on the columns the simulator moves: a request's parameters
-- and an authorization's amount are fixed once written.
GRANT USAGE ON SCHEMA simulator TO ledgerline_app;
GRANT SELECT, INSERT ON simulator.requests, simulator.authorizations TO ledgerline_app;
GRANT UPDATE (calls, outcome, reason, updated_at) ON simulator.requests TO ledgerline_app;
GRANT UPDATE (state, amount_captured, updated_at) ON simulator.authorizations TO ledgerline_app;
