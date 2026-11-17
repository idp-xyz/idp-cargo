-- IDP-Cargo SEA V0.2 (sea freight: the carrier's facts) / PostgreSQL 16.
-- Run only as the migrator role, never at service start.
-- A carrier fact about the movement: loaded on board, departed, rolled or discharged. It names a live container of
-- the movement or its current master bill or a live house's HBL, and the vessel and voyage the carrier stated.
-- The movement's state does not change, including when it is FINAL, so the fact is not a new revision of the
-- movement. The source reference is recorded once in the tenant. 000001 through 000006 are unchanged.

ALTER TABLE cargo_sea.command_receipt ADD COLUMN result_fact_no integer CHECK (result_fact_no > 0);
ALTER TABLE cargo_sea.command_receipt ADD CONSTRAINT command_receipt_fact_of_movement
  CHECK (result_fact_no IS NULL OR result_type = 'OceanMovement');

CREATE TABLE cargo_sea.ocean_carrier_fact (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 fact_no integer NOT NULL CHECK (fact_no > 0),
 kind text NOT NULL CHECK (kind IN ('LOADED_ON_BOARD','DEPARTED','ROLLED','DISCHARGED')),
 container_number text,
 bill_number text,
 vessel text NOT NULL CHECK (length(btrim(vessel)) BETWEEN 1 AND 35 AND vessel = btrim(vessel)),
 voyage text NOT NULL CHECK (voyage ~ '^[0-9A-Z][0-9A-Z/-]{0,16}$'),
 source text NOT NULL CHECK (length(btrim(source)) BETWEEN 1 AND 64 AND source = btrim(source)),
 reference text NOT NULL CHECK (length(btrim(reference)) BETWEEN 1 AND 128 AND reference = btrim(reference)),
 content_sha256 char(64) NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
 occurred_at timestamptz NOT NULL,
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, movement_id, fact_no),
 CONSTRAINT ocean_carrier_fact_source UNIQUE (tenant_id, source, reference),
 FOREIGN KEY (tenant_id, movement_id) REFERENCES cargo_sea.ocean_movement (tenant_id, movement_id),
 CONSTRAINT ocean_carrier_fact_subject CHECK ((container_number IS NOT NULL) <> (bill_number IS NOT NULL)),
 CONSTRAINT ocean_carrier_fact_container CHECK (container_number IS NULL OR cargo_sea.iso6346(container_number)),
 CONSTRAINT ocean_carrier_fact_bill CHECK (bill_number IS NULL OR (bill_number ~ '^[0-9A-Z]+([/-][0-9A-Z]+)*$' AND length(bill_number) <= 35)),
 CONSTRAINT ocean_carrier_fact_times CHECK (occurred_at <= recorded_at)
);
CREATE TRIGGER ocean_carrier_fact_immutable BEFORE UPDATE OR DELETE ON cargo_sea.ocean_carrier_fact
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.reject_row_change();

-- At commit the fact belongs to an open or final movement. A container it names is a live visit of that movement.
-- A bill it names is the movement's current master bill or a live house's HBL. Facts are numbered without gaps.
CREATE FUNCTION cargo_sea.check_ocean_carrier_fact_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  m cargo_sea.ocean_movement;
  n int;
  top int;
BEGIN
  SELECT * INTO m FROM cargo_sea.ocean_movement WHERE tenant_id = NEW.tenant_id AND movement_id = NEW.movement_id FOR SHARE;
  IF m.state IS DISTINCT FROM 'OPEN' AND m.state IS DISTINCT FROM 'FINAL' THEN
    RAISE EXCEPTION 'cargo_sea: carrier fact %/%/% is recorded while the movement is open or final', NEW.tenant_id, NEW.movement_id, NEW.fact_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_carrier_fact_consistent';
  END IF;
  IF NEW.container_number IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_sea.container_visit v
       WHERE v.tenant_id = NEW.tenant_id AND v.movement_id = NEW.movement_id AND v.container_number = NEW.container_number
         AND v.status <> 'RELEASED') THEN
    RAISE EXCEPTION 'cargo_sea: carrier fact %/%/% names a live container of its movement', NEW.tenant_id, NEW.movement_id, NEW.fact_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_carrier_fact_consistent';
  END IF;
  IF NEW.bill_number IS NOT NULL AND NOT EXISTS (
       SELECT 1 FROM cargo_sea.master_bill b WHERE b.tenant_id = NEW.tenant_id AND b.movement_id = NEW.movement_id
         AND b.status = 'CURRENT' AND b.bill_number = NEW.bill_number
       UNION ALL
       SELECT 1 FROM cargo_sea.house_bill h WHERE h.tenant_id = NEW.tenant_id AND h.movement_id = NEW.movement_id
         AND h.ended_at IS NULL AND h.hbl = NEW.bill_number) THEN
    RAISE EXCEPTION 'cargo_sea: carrier fact %/%/% names the current master bill or a live house bill', NEW.tenant_id, NEW.movement_id, NEW.fact_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_carrier_fact_consistent';
  END IF;
  SELECT count(*), coalesce(max(f.fact_no), 0) INTO n, top FROM cargo_sea.ocean_carrier_fact f
    WHERE f.tenant_id = NEW.tenant_id AND f.movement_id = NEW.movement_id;
  IF top <> n THEN
    RAISE EXCEPTION 'cargo_sea: carrier facts of %/% are numbered without gaps', NEW.tenant_id, NEW.movement_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_carrier_fact_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER ocean_carrier_fact_consistent AFTER INSERT ON cargo_sea.ocean_carrier_fact
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_ocean_carrier_fact_at_commit();

ALTER TABLE cargo_sea.ocean_carrier_fact ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_sea.ocean_carrier_fact FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_sea.ocean_carrier_fact
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
