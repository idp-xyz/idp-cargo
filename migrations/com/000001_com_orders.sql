-- IDP-Cargo COM / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The customer is an ORG party and the quote a RATE QuoteSnapshot v1, both referenced by id and revision
-- and copied as snapshots; there are no cross-schema foreign keys.
CREATE SCHEMA IF NOT EXISTS cargo_com;
CREATE TABLE cargo_com.customer_order (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 order_id text NOT NULL CHECK (length(btrim(order_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 customer_party_id text NOT NULL CHECK (length(btrim(customer_party_id)) BETWEEN 1 AND 128),
 customer_party_revision bigint NOT NULL CHECK (customer_party_revision > 0),
 customer_legal_name text NOT NULL CHECK (length(btrim(customer_legal_name)) BETWEEN 1 AND 500),
 customer_country_code char(2) NOT NULL CHECK (customer_country_code ~ '^[A-Z]{2}$'),
 customer_role_valid_from timestamptz NOT NULL,
 customer_role_valid_until timestamptz,
 customer_role_evidence_ref text NOT NULL CHECK (length(btrim(customer_role_evidence_ref)) > 0),
 quote_id text NOT NULL CHECK (length(btrim(quote_id)) BETWEEN 1 AND 128),
 quote_revision bigint NOT NULL CHECK (quote_revision > 0),
 quote_customer_revision bigint NOT NULL CHECK (quote_customer_revision > 0),
 quote_currency char(3) NOT NULL CHECK (quote_currency ~ '^[A-Z]{3}$'),
 quote_total_minor bigint NOT NULL CHECK (quote_total_minor > 0),
 quote_valid_from timestamptz NOT NULL,
 quote_valid_until timestamptz NOT NULL,
 quote_accepted_at timestamptz NOT NULL,
 quote_acceptance_evidence_ref text NOT NULL CHECK (length(btrim(quote_acceptance_evidence_ref)) > 0),
 accepted_terms_sha256 char(64) NOT NULL CHECK (accepted_terms_sha256 ~ '^[0-9a-f]{64}$'),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 status text NOT NULL CHECK (status IN ('RECORDED','PARTIALLY_ALLOCATED','ALLOCATED')),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, order_id),
 -- Candidate rule: one accepted quote backs at most one customer order.
 CONSTRAINT customer_order_one_per_quote UNIQUE (tenant_id, quote_id),
 CHECK (customer_party_revision >= quote_customer_revision),
 CHECK (quote_valid_from < quote_valid_until),
 CHECK (quote_accepted_at >= quote_valid_from AND quote_accepted_at < quote_valid_until),
 CHECK (recorded_at >= quote_accepted_at AND recorded_at < quote_valid_until),
 CHECK (customer_role_valid_until IS NULL OR customer_role_valid_from < customer_role_valid_until),
 CHECK (recorded_at >= customer_role_valid_from AND (customer_role_valid_until IS NULL OR recorded_at < customer_role_valid_until))
);
CREATE TABLE cargo_com.order_line (
 tenant_id text NOT NULL,
 order_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 description text NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 500),
 unit_code text NOT NULL CHECK (unit_code ~ '^[A-Z0-9]{1,3}$'),
 quantity bigint NOT NULL CHECK (quantity > 0),
 PRIMARY KEY (tenant_id, order_id, line_no),
 FOREIGN KEY (tenant_id, order_id) REFERENCES cargo_com.customer_order (tenant_id, order_id)
);
-- Every version of an obligation is its own immutable row; S1 creates version 1 only.
CREATE TABLE cargo_com.fulfillment_obligation (
 tenant_id text NOT NULL,
 obligation_id text NOT NULL CHECK (length(btrim(obligation_id)) BETWEEN 1 AND 128),
 obligation_version bigint NOT NULL CHECK (obligation_version > 0),
 order_id text NOT NULL,
 line_no integer NOT NULL,
 unit_code text NOT NULL,
 quantity bigint NOT NULL CHECK (quantity > 0),
 allocated_by text NOT NULL CHECK (length(btrim(allocated_by)) > 0),
 allocated_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, obligation_id, obligation_version),
 FOREIGN KEY (tenant_id, order_id, line_no) REFERENCES cargo_com.order_line (tenant_id, order_id, line_no)
);
CREATE INDEX fulfillment_obligation_by_line ON cargo_com.fulfillment_obligation (tenant_id, order_id, line_no);
CREATE TABLE cargo_com.command_receipt (
 tenant_id text NOT NULL,
 command_id text NOT NULL,
 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 result_type text NOT NULL,
 result_id text NOT NULL,
 result_revision bigint NOT NULL CHECK (result_revision > 0),
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, command_id)
);
-- The commitment of an order never changes: only status, revision and updated_at may, and the status only
-- moves forward. Orders are never deleted.
CREATE FUNCTION cargo_com.reject_order_commitment_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  rank_old int;
  rank_new int;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_com: order %/% cannot be deleted', OLD.tenant_id, OLD.order_id USING ERRCODE = 'restrict_violation';
  END IF;
  rank_old := array_position(ARRAY['RECORDED','PARTIALLY_ALLOCATED','ALLOCATED'], OLD.status);
  rank_new := array_position(ARRAY['RECORDED','PARTIALLY_ALLOCATED','ALLOCATED'], NEW.status);
  IF to_jsonb(OLD) - ARRAY['status','revision','updated_at'] IS DISTINCT FROM to_jsonb(NEW) - ARRAY['status','revision','updated_at']
     OR rank_new < rank_old THEN
    RAISE EXCEPTION 'cargo_com: the commitment of order %/% is immutable', OLD.tenant_id, OLD.order_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER customer_order_commitment_immutable BEFORE UPDATE OR DELETE ON cargo_com.customer_order
  FOR EACH ROW EXECUTE FUNCTION cargo_com.reject_order_commitment_change();
CREATE FUNCTION cargo_com.reject_row_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_com: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER order_line_immutable BEFORE UPDATE OR DELETE ON cargo_com.order_line
  FOR EACH ROW EXECUTE FUNCTION cargo_com.reject_row_change();
CREATE TRIGGER fulfillment_obligation_immutable BEFORE UPDATE OR DELETE ON cargo_com.fulfillment_obligation
  FOR EACH ROW EXECUTE FUNCTION cargo_com.reject_row_change();
-- An obligation must fit its line: same unit, and the line's obligations never exceed its quantity. The
-- order row is locked first so that concurrent inserts for one order are checked one after another.
CREATE FUNCTION cargo_com.check_obligation_within_line() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  line_unit text;
  line_quantity bigint;
  allocated bigint;
BEGIN
  PERFORM 1 FROM cargo_com.customer_order WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id FOR NO KEY UPDATE;
  SELECT unit_code, quantity INTO line_unit, line_quantity FROM cargo_com.order_line
    WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id AND line_no = NEW.line_no;
  SELECT coalesce(sum(quantity), 0) INTO allocated FROM cargo_com.fulfillment_obligation
    WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id AND line_no = NEW.line_no;
  IF line_unit IS DISTINCT FROM NEW.unit_code OR allocated + NEW.quantity > line_quantity THEN
    RAISE EXCEPTION 'cargo_com: obligation %/% does not fit order line %', NEW.tenant_id, NEW.obligation_id, NEW.line_no
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER fulfillment_obligation_within_line BEFORE INSERT ON cargo_com.fulfillment_obligation
  FOR EACH ROW EXECUTE FUNCTION cargo_com.check_obligation_within_line();
-- Tenant isolation as in cargo_org and cargo_rate: the tenant transactor sets app.tenant_id in every transaction.
DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['customer_order','order_line','fulfillment_obligation','command_receipt'] LOOP
    EXECUTE format('ALTER TABLE cargo_com.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_com.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_com.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
