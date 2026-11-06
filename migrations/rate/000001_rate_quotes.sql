-- IDP-Cargo RATE / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The customer is an ORG party referenced by id and revision; there are no cross-schema foreign keys.
CREATE SCHEMA IF NOT EXISTS cargo_rate;
CREATE TABLE cargo_rate.quote (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 quote_id text NOT NULL CHECK (length(btrim(quote_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 customer_party_id text NOT NULL CHECK (length(btrim(customer_party_id)) BETWEEN 1 AND 128),
 customer_party_revision bigint NOT NULL CHECK (customer_party_revision > 0),
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 valid_from timestamptz NOT NULL,
 valid_until timestamptz NOT NULL,
 margin_floor_bp integer NOT NULL CHECK (margin_floor_bp BETWEEN 0 AND 10000),
 author text NOT NULL CHECK (length(btrim(author)) > 0),
 status text NOT NULL CHECK (status IN ('AWAITING_APPROVAL','PRICED','ACCEPTED')),
 approved_by text,
 approved_at timestamptz,
 approval_reason_ref text,
 accepted_by text,
 accepted_at timestamptz,
 acceptance_evidence_ref text,
 accepted_customer_revision bigint,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, quote_id),
 CHECK (valid_from < valid_until),
 CHECK ((approved_by IS NULL) = (approved_at IS NULL) AND (approved_by IS NULL) = (approval_reason_ref IS NULL)),
 CHECK (approved_by IS NULL OR approved_by <> author),
 CHECK ((status = 'ACCEPTED') = (accepted_at IS NOT NULL)),
 CHECK (status <> 'ACCEPTED' OR (accepted_by IS NOT NULL AND acceptance_evidence_ref IS NOT NULL
        AND accepted_customer_revision >= customer_party_revision
        AND accepted_at >= valid_from AND accepted_at < valid_until))
);
CREATE TABLE cargo_rate.quote_line (
 tenant_id text NOT NULL,
 quote_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 charge_code text NOT NULL CHECK (length(btrim(charge_code)) BETWEEN 1 AND 64),
 sell_minor bigint NOT NULL CHECK (sell_minor > 0),
 cost_minor bigint NOT NULL CHECK (cost_minor >= 0),
 PRIMARY KEY (tenant_id, quote_id, line_no),
 FOREIGN KEY (tenant_id, quote_id) REFERENCES cargo_rate.quote (tenant_id, quote_id)
);
CREATE TABLE cargo_rate.command_receipt (
 tenant_id text NOT NULL,
 command_id text NOT NULL,
 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 result_type text NOT NULL,
 result_id text NOT NULL,
 result_revision bigint NOT NULL CHECK (result_revision > 0),
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, command_id)
);
-- An accepted quote is QuoteSnapshot v1 and never changes; quote lines never change once written.
CREATE FUNCTION cargo_rate.reject_accepted_quote_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF OLD.status = 'ACCEPTED' THEN
    RAISE EXCEPTION 'cargo_rate: accepted quote %/% is immutable', OLD.tenant_id, OLD.quote_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER quote_accepted_immutable BEFORE UPDATE OR DELETE ON cargo_rate.quote
  FOR EACH ROW EXECUTE FUNCTION cargo_rate.reject_accepted_quote_change();
CREATE FUNCTION cargo_rate.reject_quote_line_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_rate: quote lines are immutable' USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER quote_line_immutable BEFORE UPDATE OR DELETE ON cargo_rate.quote_line
  FOR EACH ROW EXECUTE FUNCTION cargo_rate.reject_quote_line_change();
-- Tenant isolation as in cargo_org: the tenant transactor sets app.tenant_id in every transaction.
DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['quote','quote_line','command_receipt'] LOOP
    EXECUTE format('ALTER TABLE cargo_rate.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_rate.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_rate.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
