-- Least privilege for ledgerline_app, the role serve and worker connect as
-- (amendment 3.9). Ledger tables get SELECT and INSERT only, so an UPDATE or
-- DELETE fails on privilege before the immutability trigger is even reached.
-- Every later migration that adds a table grants on it explicitly.

GRANT USAGE ON SCHEMA public TO ledgerline_app;

GRANT SELECT, INSERT ON merchants, api_keys, customers, ledger_accounts,
    ledger_transactions, ledger_entries, events TO ledgerline_app;
GRANT SELECT, INSERT, UPDATE ON payment_intents, refunds, webhook_endpoints,
    webhook_deliveries TO ledgerline_app;
-- Expired keys are deleted by the cleanup job.
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO ledgerline_app;
