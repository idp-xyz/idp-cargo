-- IDP-Cargo FUL V0.4, stage 4 (V2.111) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- With every load evidence version, the shipment's pieces on each flight or voyage of the consolidation's tenders, as PHY
-- attributes the carrier's evidence flight by flight (PhysicalLoadEvidence v1): the transport as reported (an air flight
-- with its date, such as MU587/02DEC), and the pieces departed, arrived and offloaded on it, in the order PHY first
-- reported them. The flights of a version add up to its departed, arrived and offloaded pieces. Rows are immutable and
-- written with their evidence version. Versions recorded before keep none.

CREATE TABLE cargo_ful.shipment_load_evidence_flight (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 evidence_no integer NOT NULL,
 flight_no integer NOT NULL CHECK (flight_no > 0),
 transport text NOT NULL CHECK (length(btrim(transport)) BETWEEN 1 AND 500),
 departed bigint NOT NULL CHECK (departed >= 0),
 arrived bigint NOT NULL CHECK (arrived >= 0),
 offloaded bigint NOT NULL CHECK (offloaded >= 0),
 PRIMARY KEY (tenant_id, shipment_id, evidence_no, flight_no),
 CONSTRAINT shipment_load_evidence_flight_once UNIQUE (tenant_id, shipment_id, evidence_no, transport),
 FOREIGN KEY (tenant_id, shipment_id, evidence_no) REFERENCES cargo_ful.shipment_load_evidence (tenant_id, shipment_id, evidence_no),
 CONSTRAINT shipment_load_evidence_flight_pieces CHECK (departed + arrived + offloaded > 0)
);

CREATE TRIGGER shipment_load_evidence_flight_immutable BEFORE UPDATE OR DELETE ON cargo_ful.shipment_load_evidence_flight
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.reject_row_change();

-- At commit, flight rows are written by the transaction that writes their evidence version (as stuffing rows), and the
-- flights of the version add up to its departed, arrived and offloaded pieces.
CREATE FUNCTION cargo_ful.check_evidence_flight_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  e cargo_ful.shipment_load_evidence;
  dep bigint;
  arr bigint;
  off bigint;
BEGIN
  SELECT * INTO e FROM cargo_ful.shipment_load_evidence x WHERE x.tenant_id = NEW.tenant_id AND x.shipment_id = NEW.shipment_id
    AND x.evidence_no = NEW.evidence_no;
  IF e.shipment_id IS NULL OR NOT EXISTS (SELECT 1 FROM cargo_ful.shipment_load_evidence x WHERE x.tenant_id = NEW.tenant_id
       AND x.shipment_id = NEW.shipment_id AND x.evidence_no = NEW.evidence_no AND x.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_ful: flights of load evidence %/%/% are written with it', NEW.tenant_id, NEW.shipment_id, NEW.evidence_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_load_evidence_flight_with_evidence';
  END IF;
  SELECT sum(f.departed), sum(f.arrived), sum(f.offloaded) INTO dep, arr, off FROM cargo_ful.shipment_load_evidence_flight f
    WHERE f.tenant_id = NEW.tenant_id AND f.shipment_id = NEW.shipment_id AND f.evidence_no = NEW.evidence_no;
  IF dep <> e.departed OR arr <> e.arrived OR off <> e.offloaded THEN
    RAISE EXCEPTION 'cargo_ful: the flights of load evidence %/%/% do not add up to its pieces', NEW.tenant_id, NEW.shipment_id, NEW.evidence_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_load_evidence_flights_add_up';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER shipment_load_evidence_flight_with_evidence AFTER INSERT ON cargo_ful.shipment_load_evidence_flight
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_evidence_flight_at_commit();

ALTER TABLE cargo_ful.shipment_load_evidence_flight ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_ful.shipment_load_evidence_flight FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_ful.shipment_load_evidence_flight
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
