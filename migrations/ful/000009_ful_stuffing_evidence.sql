-- IDP-Cargo FUL V0.4 (sea freight part 1c) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- What SEA needs of a shipment's stuffing for OceanOperationPlan v1 (decision N1-PHY OD-2, option A): with every load
-- evidence version, the sealed containers of the attached consolidation with packs of the shipment, as PHY verified
-- them (PhysicalLoadEvidence v1, PHY V0.3): the stuffing, the planned unit's number and ISO 6346 size-type, the
-- shipment's packs, gross weight and volume in it, and the seal PHY recorded as its closing evidence. The container's
-- number and plate are SEA's. Rows are immutable and written with their evidence version; versions recorded before
-- keep none.

CREATE TABLE cargo_ful.shipment_load_evidence_stuffing (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 evidence_no integer NOT NULL,
 stuffing_no integer NOT NULL CHECK (stuffing_no > 0),
 execution_id text NOT NULL CHECK (length(btrim(execution_id)) BETWEEN 1 AND 128),
 load_unit_no integer NOT NULL CHECK (load_unit_no > 0),
 type_code text NOT NULL CHECK (type_code ~ '^[0-9A-Z]{2}[A-Z][0-9A-Z]$'),
 packs bigint NOT NULL CHECK (packs > 0),
 gross_weight_unit text CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint NOT NULL CHECK (gross_weight >= 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint NOT NULL CHECK (volume >= 0),
 seal text NOT NULL CHECK (length(btrim(seal)) BETWEEN 1 AND 500),
 sealed_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, shipment_id, evidence_no, stuffing_no),
 FOREIGN KEY (tenant_id, shipment_id, evidence_no) REFERENCES cargo_ful.shipment_load_evidence (tenant_id, shipment_id, evidence_no),
 CONSTRAINT shipment_load_evidence_stuffing_units CHECK ((gross_weight_unit IS NOT NULL OR gross_weight = 0) AND (volume_unit IS NOT NULL OR volume = 0))
);

CREATE TRIGGER shipment_load_evidence_stuffing_immutable BEFORE UPDATE OR DELETE ON cargo_ful.shipment_load_evidence_stuffing
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.reject_row_change();

-- At commit, stuffing rows are written by the transaction that writes their evidence version (as screening rows).
CREATE FUNCTION cargo_ful.check_evidence_stuffing_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_ful.shipment_load_evidence e WHERE e.tenant_id = NEW.tenant_id AND e.shipment_id = NEW.shipment_id
       AND e.evidence_no = NEW.evidence_no AND e.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_ful: stuffings of load evidence %/%/% are written with it', NEW.tenant_id, NEW.shipment_id, NEW.evidence_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_load_evidence_stuffing_with_evidence';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER shipment_load_evidence_stuffing_with_evidence AFTER INSERT ON cargo_ful.shipment_load_evidence_stuffing
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_evidence_stuffing_at_commit();

ALTER TABLE cargo_ful.shipment_load_evidence_stuffing ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_ful.shipment_load_evidence_stuffing FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_ful.shipment_load_evidence_stuffing
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
