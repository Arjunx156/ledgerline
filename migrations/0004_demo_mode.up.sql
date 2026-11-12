-- Demo mode (amendment 3.13): throwaway sandbox merchants, and the trace
-- stream the proof console reads. Only `ledgerline demo` writes these.

ALTER TABLE merchants ADD COLUMN sandbox boolean NOT NULL DEFAULT false;
-- Lets demo_sessions require a sandbox merchant through a foreign key.
ALTER TABLE merchants ADD CONSTRAINT merchants_id_sandbox_key UNIQUE (id, sandbox);

-- A session is one visitor's sandbox merchant for an hour. Its id is the
-- read capability for the trace stream; moving money takes the merchant's
-- secret key. last_seq hands out trace numbers under this row's lock, so a
-- session's events commit in seq order and a reader resuming after the
-- highest seq it saw never skips one.
CREATE TABLE demo_sessions (
    id          text PRIMARY KEY,
    merchant_id text NOT NULL UNIQUE,
    sandbox     boolean NOT NULL DEFAULT true CHECK (sandbox),
    expires_at  timestamptz NOT NULL,
    last_seq    bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (merchant_id, sandbox) REFERENCES merchants (id, sandbox)
);

-- One row per trace event, in the shape frozen at S2 (internal/trace).
CREATE TABLE demo_trace_events (
    session    text NOT NULL REFERENCES demo_sessions (id),
    seq        bigint NOT NULL CHECK (seq >= 1),
    request_id text NOT NULL,
    at         timestamptz NOT NULL,
    lane       text NOT NULL CHECK (lane IN ('client', 'api', 'postgres', 'processor', 'worker', 'receiver')),
    site       text NOT NULL,
    payment_id text,
    key        text,
    data       jsonb,
    PRIMARY KEY (session, seq)
);

GRANT SELECT, INSERT ON demo_sessions, demo_trace_events TO ledgerline_app;
GRANT UPDATE (last_seq) ON demo_sessions TO ledgerline_app;
