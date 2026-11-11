-- IDP-Cargo FUL V0.4 (third part) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- What AIR needs to finalize its document facts for DOC (AirDocFacts v1): the goods description of the order
-- line a planned obligation entry comes from, as COM reported it when FUL planned the version (entries planned
-- before keep none), and the latest screening result of each lot of an AIR shipment, as PHY recorded it, with
-- every load evidence version. Rows are immutable and written with their evidence version.

ALTER TABLE cargo_ful.shipment_obligation
  ADD COLUMN goods_description text CHECK (length(btrim(goods_description)) BETWEEN 1 AND 500);

CREATE TABLE cargo_ful.shipment_load_evidence_screening (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 evidence_no integer NOT NULL,
 screening_no integer NOT NULL CHECK (screening_no > 0),
 lot_id text NOT NULL CHECK (length(btrim(lot_id)) BETWEEN 1 AND 128),
 outcome text NOT NULL CHECK (outcome IN ('PASSED','FAILED','PENDING','NOT_REQUIRED')),
 screener text NOT NULL CHECK (length(btrim(screener)) BETWEEN 1 AND 500),
 method text NOT NULL CHECK (length(btrim(method)) BETWEEN 1 AND 128),
 reference text NOT NULL CHECK (length(btrim(reference)) BETWEEN 1 AND 500),
 screened_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, shipment_id, evidence_no, screening_no),
 FOREIGN KEY (tenant_id, shipment_id, evidence_no) REFERENCES cargo_ful.shipment_load_evidence (tenant_id, shipment_id, evidence_no)
);

CREATE TRIGGER shipment_load_evidence_screening_immutable BEFORE UPDATE OR DELETE ON cargo_ful.shipment_load_evidence_screening
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.reject_row_change();

-- At commit, screening rows are written by the transaction that writes their evidence version.
CREATE FUNCTION cargo_ful.check_evidence_screening_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_ful.shipment_load_evidence e WHERE e.tenant_id = NEW.tenant_id AND e.shipment_id = NEW.shipment_id
       AND e.evidence_no = NEW.evidence_no AND e.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_ful: screenings of load evidence %/%/% are written with it', NEW.tenant_id, NEW.shipment_id, NEW.evidence_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_load_evidence_screening_with_evidence';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER shipment_load_evidence_screening_with_evidence AFTER INSERT ON cargo_ful.shipment_load_evidence_screening
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_evidence_screening_at_commit();

ALTER TABLE cargo_ful.shipment_load_evidence_screening ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_ful.shipment_load_evidence_screening FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_ful.shipment_load_evidence_screening
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
