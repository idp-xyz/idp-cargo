-- IDP-Cargo RATE V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Business review decisions R3, R4, R5 and R7, owner decisions R8, R9 and T1 (2026-11-09), and the quote side
-- of C8:
--   R3 a designated role may approve its own quote, flagged;  R4 quotes not accepted expire after validity;
--   R5 acceptance evidence has a kind;  R7 quotes go to customers, overseas agents, shippers or consignees;
--   R8 each line and cost has its currency, totalled in the quote currency at frozen exchange rates;
--   R9 zero lines, and discounts as negative lines without cost;  T1 the acceptance's recording time is kept;
--   C8 optional Incoterms with edition and named place, and a service level.
-- V0.2 quotes convert without loss: their lines are in the quote currency, their recipient a customer. An
-- accepted V0.2 quote gets the evidence kind UNRECORDED and no recording time, since neither was kept.

ALTER TABLE cargo_rate.quote
  ADD COLUMN recipient_role text NOT NULL DEFAULT 'CUSTOMER' CHECK (recipient_role IN ('CUSTOMER','OVERSEAS_AGENT','SHIPPER','CONSIGNEE')),
  ADD COLUMN incoterms_rule text CHECK (incoterms_rule ~ '^[A-Z]{3}$'),
  ADD COLUMN incoterms_version text CHECK (incoterms_version IN ('2010','2020')),
  ADD COLUMN named_place text CHECK (length(btrim(named_place)) BETWEEN 1 AND 200),
  ADD COLUMN service_level text CHECK (length(btrim(service_level)) BETWEEN 1 AND 64),
  ADD COLUMN self_approved boolean NOT NULL DEFAULT false,
  ADD COLUMN acceptance_evidence_kind text CHECK (acceptance_evidence_kind IN ('SIGNED_DOCUMENT','EMAIL','CUSTOMER_PORTAL','UNRECORDED')),
  ADD COLUMN accepted_recorded_at timestamptz,
  ADD COLUMN expired_at timestamptz;
ALTER TABLE cargo_rate.quote_line
  ADD COLUMN sell_currency char(3),
  ADD COLUMN cost_currency char(3);

-- Accepted quotes and lines are immutable to the application, and both tables force row level security, so
-- the conversion runs with those triggers off and the owner exempt for its duration.
ALTER TABLE cargo_rate.quote NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_rate.quote_line NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_rate.quote DISABLE TRIGGER quote_accepted_immutable;
ALTER TABLE cargo_rate.quote_line DISABLE TRIGGER quote_line_immutable;
UPDATE cargo_rate.quote SET acceptance_evidence_kind = 'UNRECORDED' WHERE status = 'ACCEPTED';
UPDATE cargo_rate.quote_line l SET sell_currency = q.currency, cost_currency = q.currency
  FROM cargo_rate.quote q WHERE q.tenant_id = l.tenant_id AND q.quote_id = l.quote_id;
ALTER TABLE cargo_rate.quote ENABLE TRIGGER quote_accepted_immutable;
ALTER TABLE cargo_rate.quote_line ENABLE TRIGGER quote_line_immutable;
ALTER TABLE cargo_rate.quote FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_rate.quote_line FORCE ROW LEVEL SECURITY;

ALTER TABLE cargo_rate.quote
  DROP CONSTRAINT quote_status_check,
  ADD CONSTRAINT quote_status_check CHECK (status IN ('AWAITING_APPROVAL','PRICED','ACCEPTED','EXPIRED')),
  DROP CONSTRAINT quote_check2,
  -- Written with CASE so that no branch evaluates to NULL, which a CHECK would let pass.
  ADD CONSTRAINT quote_self_approval CHECK (CASE WHEN approved_by IS NULL THEN NOT self_approved
    ELSE self_approved = (approved_by = author) END),
  ADD CONSTRAINT quote_acceptance_evidence CHECK (CASE WHEN status = 'ACCEPTED'
    THEN acceptance_evidence_kind IS NOT NULL AND (acceptance_evidence_kind = 'UNRECORDED') = (accepted_recorded_at IS NULL)
      AND (accepted_recorded_at IS NULL OR accepted_recorded_at >= accepted_at - interval '5 minutes')
    ELSE acceptance_evidence_kind IS NULL AND accepted_recorded_at IS NULL END),
  ADD CONSTRAINT quote_expiry CHECK ((status = 'EXPIRED') = (expired_at IS NOT NULL) AND (expired_at IS NULL OR expired_at >= valid_until)),
  ADD CONSTRAINT quote_terms CHECK ((incoterms_rule IS NULL) = (incoterms_version IS NULL) AND (incoterms_rule IS NULL) = (named_place IS NULL));
ALTER TABLE cargo_rate.quote_line
  ALTER COLUMN sell_currency SET NOT NULL,
  ALTER COLUMN cost_currency SET NOT NULL,
  ADD CONSTRAINT quote_line_currencies CHECK (sell_currency ~ '^[A-Z]{3}$' AND cost_currency ~ '^[A-Z]{3}$'),
  DROP CONSTRAINT quote_line_sell_minor_check,
  ADD CONSTRAINT quote_line_discount CHECK (sell_minor >= 0 OR cost_minor = 0);

-- R8: the frozen rates of a quote, one per currency other than the quote currency; immutable like lines.
CREATE TABLE cargo_rate.quote_exchange_rate (
 tenant_id text NOT NULL,
 quote_id text NOT NULL,
 from_currency char(3) NOT NULL CHECK (from_currency ~ '^[A-Z]{3}$'),
 rate bigint NOT NULL CHECK (rate > 0),
 scale smallint NOT NULL CHECK (scale BETWEEN 0 AND 12),
 from_digits smallint NOT NULL CHECK (from_digits BETWEEN 0 AND 4),
 quote_digits smallint NOT NULL CHECK (quote_digits BETWEEN 0 AND 4),
 source text NOT NULL CHECK (length(btrim(source)) BETWEEN 1 AND 200),
 as_of timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, quote_id, from_currency),
 FOREIGN KEY (tenant_id, quote_id) REFERENCES cargo_rate.quote (tenant_id, quote_id)
);
CREATE FUNCTION cargo_rate.reject_row_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_rate: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER quote_exchange_rate_immutable BEFORE UPDATE OR DELETE ON cargo_rate.quote_exchange_rate
  FOR EACH ROW EXECUTE FUNCTION cargo_rate.reject_row_change();
ALTER TABLE cargo_rate.quote_exchange_rate ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_rate.quote_exchange_rate FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_rate.quote_exchange_rate
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- An accepted or expired quote never changes.
CREATE OR REPLACE FUNCTION cargo_rate.reject_accepted_quote_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF OLD.status IN ('ACCEPTED','EXPIRED') THEN
    RAISE EXCEPTION 'cargo_rate: % quote %/% is immutable', lower(OLD.status), OLD.tenant_id, OLD.quote_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END $fn$;
