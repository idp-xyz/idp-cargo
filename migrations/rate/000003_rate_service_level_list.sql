-- IDP-Cargo RATE V0.4 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Decision C8 (amended 2026-11-09): a service level is a code from RATE's service-level list, no longer free
-- text. The list is reference data the application holds (S1: STANDARD, EXPRESS, ECONOMY, DEFERRED); the
-- database checks the form of a code. A service level recorded before the list existed is kept as recorded
-- and marked unlisted; it is never accepted on a new quote.
ALTER TABLE cargo_rate.quote ADD COLUMN service_level_unlisted boolean NOT NULL DEFAULT false;

-- Quotes are immutable once accepted and the table forces row level security, so the marking runs with the
-- immutability trigger off and the owner exempt for its duration.
ALTER TABLE cargo_rate.quote NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_rate.quote DISABLE TRIGGER quote_accepted_immutable;
UPDATE cargo_rate.quote SET service_level_unlisted = true
  WHERE service_level IS NOT NULL AND service_level NOT IN ('STANDARD','EXPRESS','ECONOMY','DEFERRED');
ALTER TABLE cargo_rate.quote ENABLE TRIGGER quote_accepted_immutable;
ALTER TABLE cargo_rate.quote FORCE ROW LEVEL SECURITY;

ALTER TABLE cargo_rate.quote
  ADD CONSTRAINT quote_service_level_code CHECK (CASE
    WHEN service_level_unlisted THEN service_level IS NOT NULL
    ELSE service_level IS NULL OR service_level ~ '^[A-Z][A-Z0-9_]{1,15}$' END);
COMMENT ON COLUMN cargo_rate.quote.service_level_unlisted IS
  'The service level was recorded as free text before the service-level list existed (C8); kept as recorded.';
