-- IDP-Cargo ORG V0.7 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- ContactTransferRegister v1 (decision N1-DOC OD-4 A, privacy decision P5): per tenant and calendar year (China
-- time), the distinct persons whose contacts DOC and BOOK transferred abroad, counted from the reservations of each
-- dispatch by contact key (kernel grammar: ORG:<party>:<contact id>, COM:<order>:<role>:<party version> or SHA:<digest>;
-- never a name, phone or e-mail); a dispatch reserves once, in one year; reservations are taken under the year's
-- register row lock and released once; standard contracts and certifications on record lift the block from 100,000
-- persons. Times are compared on ORG's clock (entered_at), never on the client's. Kept three years (P6; the purge
-- comes with the platform scheduler).

CREATE TABLE cargo_org.contact_transfer_register (
  tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
  year integer NOT NULL CHECK (year BETWEEN 2000 AND 2999),
  revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
  PRIMARY KEY (tenant_id, year)
);

-- A dispatch reserves once, in one year: this row is its marker, written with its reservations.
CREATE TABLE cargo_org.contact_transfer_dispatch (
  tenant_id text NOT NULL,
  dispatch_id text NOT NULL CHECK (length(btrim(dispatch_id)) BETWEEN 1 AND 128),
  year integer NOT NULL,
  reserved_by text NOT NULL CHECK (length(btrim(reserved_by)) > 0),
  reserved_at timestamptz NOT NULL,
  entered_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, dispatch_id),
  UNIQUE (tenant_id, dispatch_id, year),
  FOREIGN KEY (tenant_id, year) REFERENCES cargo_org.contact_transfer_register (tenant_id, year)
);

CREATE TABLE cargo_org.contact_transfer_reservation (
  tenant_id text NOT NULL,
  year integer NOT NULL,
  dispatch_id text NOT NULL,
  contact_key text NOT NULL CHECK (contact_key ~ '^(ORG:[A-Za-z0-9._-]{1,128}:[A-Za-z0-9._-]{1,128}|COM:[A-Za-z0-9._-]{1,128}:(SHIPPER|CONSIGNEE|NOTIFY_PARTY):[1-9][0-9]{0,9}|SHA:[0-9a-f]{64})$'),
  reserved_by text NOT NULL CHECK (length(btrim(reserved_by)) > 0),
  reserved_at timestamptz NOT NULL,
  entered_at timestamptz NOT NULL,
  released_by text CHECK (length(btrim(released_by)) > 0),
  released_at timestamptz,
  release_entered_at timestamptz,
  PRIMARY KEY (tenant_id, year, dispatch_id, contact_key),
  FOREIGN KEY (tenant_id, dispatch_id, year) REFERENCES cargo_org.contact_transfer_dispatch (tenant_id, dispatch_id, year),
  CONSTRAINT contact_transfer_release CHECK ((released_at IS NULL) = (released_by IS NULL) AND (released_at IS NULL) = (release_entered_at IS NULL)
    AND (release_entered_at IS NULL OR release_entered_at >= entered_at))
);
CREATE INDEX contact_transfer_live_keys ON cargo_org.contact_transfer_reservation (tenant_id, year, contact_key) WHERE released_at IS NULL;
CREATE INDEX contact_transfer_by_dispatch ON cargo_org.contact_transfer_reservation (tenant_id, dispatch_id) WHERE released_at IS NULL;

CREATE TABLE cargo_org.contact_transfer_safeguard (
  tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
  safeguard_id text NOT NULL CHECK (length(btrim(safeguard_id)) BETWEEN 1 AND 128),
  kind text NOT NULL CHECK (kind IN ('STANDARD_CONTRACT','CERTIFICATION')),
  reference text NOT NULL CHECK (length(btrim(reference)) BETWEEN 1 AND 500),
  valid_from timestamptz NOT NULL,
  valid_until timestamptz,
  evidence_ref text NOT NULL CHECK (length(btrim(evidence_ref)) BETWEEN 1 AND 500),
  recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
  recorded_at timestamptz NOT NULL,
  entered_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, safeguard_id),
  CONSTRAINT contact_transfer_safeguard_validity CHECK (valid_until IS NULL OR valid_until > valid_from)
);

-- Receipts of reservations and releases keep the persons counted and the state they answered.
ALTER TABLE cargo_org.command_receipt
  ADD COLUMN result_persons bigint CHECK (result_persons >= 0),
  ADD COLUMN result_state text CHECK (result_state IN ('OK','WARNING','BLOCKED')),
  ADD CONSTRAINT command_receipt_register CHECK ((result_persons IS NULL) = (result_state IS NULL));

-- The register changes only its revision; a reservation is only released, once; a dispatch's marker and a safeguard
-- never change.
CREATE FUNCTION cargo_org.contact_transfer_register_revision_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR NEW.tenant_id <> OLD.tenant_id OR NEW.year <> OLD.year OR NEW.revision <= OLD.revision THEN
    RAISE EXCEPTION 'cargo_org: contact transfer register %/% only moves its revision on', OLD.tenant_id, OLD.year USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER contact_transfer_register_rules BEFORE UPDATE OR DELETE ON cargo_org.contact_transfer_register
  FOR EACH ROW EXECUTE FUNCTION cargo_org.contact_transfer_register_revision_only();

CREATE FUNCTION cargo_org.contact_transfer_release_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  releases text[] := ARRAY['released_by','released_at','release_entered_at'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.released_at IS NOT NULL OR NEW.released_at IS NULL OR to_jsonb(OLD) - releases IS DISTINCT FROM to_jsonb(NEW) - releases THEN
    RAISE EXCEPTION 'cargo_org: contact transfer reservation %/%/% can only be released, once', OLD.tenant_id, OLD.dispatch_id, OLD.contact_key
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER contact_transfer_reservation_rules BEFORE UPDATE OR DELETE ON cargo_org.contact_transfer_reservation
  FOR EACH ROW EXECUTE FUNCTION cargo_org.contact_transfer_release_only();

CREATE FUNCTION cargo_org.contact_transfer_immutable() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_org: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER contact_transfer_safeguard_immutable BEFORE UPDATE OR DELETE ON cargo_org.contact_transfer_safeguard
  FOR EACH ROW EXECUTE FUNCTION cargo_org.contact_transfer_immutable();
CREATE TRIGGER contact_transfer_dispatch_immutable BEFORE UPDATE OR DELETE ON cargo_org.contact_transfer_dispatch
  FOR EACH ROW EXECUTE FUNCTION cargo_org.contact_transfer_immutable();

-- At commit, a reservation or release is written by the transaction that moved its year's register on, under the
-- register row's lock: no reservation is counted past the threshold check.
CREATE FUNCTION cargo_org.check_contact_transfer_under_lock() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_org.contact_transfer_register r WHERE r.tenant_id = NEW.tenant_id AND r.year = NEW.year
       AND r.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_org: % of dispatch %/% is written under its register''s lock', TG_TABLE_NAME, NEW.tenant_id, NEW.dispatch_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contact_transfer_under_lock';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER contact_transfer_under_lock AFTER INSERT OR UPDATE ON cargo_org.contact_transfer_reservation
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_contact_transfer_under_lock();
CREATE CONSTRAINT TRIGGER contact_transfer_dispatch_under_lock AFTER INSERT ON cargo_org.contact_transfer_dispatch
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_contact_transfer_under_lock();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['contact_transfer_register','contact_transfer_dispatch','contact_transfer_reservation','contact_transfer_safeguard'] LOOP
    EXECUTE format('ALTER TABLE cargo_org.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_org.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format('CREATE POLICY tenant_isolation ON cargo_org.%I USING (tenant_id = current_setting(''app.tenant_id'', true)) '
      'WITH CHECK (tenant_id = current_setting(''app.tenant_id'', true))', tbl);
  END LOOP;
END $rls$;
