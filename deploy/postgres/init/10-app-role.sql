-- The role serve and worker connect as. It owns nothing; migration 0002 grants
-- it the minimum. The password is for local development only: a deployment
-- creates this role with its own secret.
CREATE ROLE ledgerline_app LOGIN PASSWORD 'ledgerline_app';
