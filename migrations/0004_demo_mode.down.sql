DROP TABLE demo_trace_events;
DROP TABLE demo_sessions;
ALTER TABLE merchants DROP CONSTRAINT merchants_id_sandbox_key;
ALTER TABLE merchants DROP COLUMN sandbox;
