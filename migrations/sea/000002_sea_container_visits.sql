-- IDP-Cargo SEA V0.2 (sea freight part 1e-i) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Container visits (N1-PHY design, aggregate ContainerVisit, backlog PB6): one use of one container for one ocean
-- movement of the consolidator's own container (SEA_LCL_OWN). The container is assigned to a planned load unit of the
-- consolidation by its ISO 6346 number; seals are recorded with the party that applied them and kept as history; the
-- visit is STUFFED once a live house of the movement carries PHY's sealed stuffing of the unit, as FUL relays it; an
-- unused container is RELEASED. The same equipment number recurs in a later visit once this one is released.
-- Not in this migration: VGM assertions with their signatory, and the container's terminal and vessel events.
-- No foreign key leaves cargo_sea: the load unit and the stuffing are PHY's.

-- A command receipt may name a container visit and, for a seal, its number in the visit's history.
ALTER TABLE cargo_sea.command_receipt DROP CONSTRAINT command_receipt_result_type_check;
ALTER TABLE cargo_sea.command_receipt ADD CONSTRAINT command_receipt_result_type_check
  CHECK (result_type IN ('OceanMovement','HblSeries','ContainerVisit'));
ALTER TABLE cargo_sea.command_receipt ADD COLUMN result_seal_no integer CHECK (result_seal_no > 0);
ALTER TABLE cargo_sea.command_receipt ADD CONSTRAINT command_receipt_seal_of_visit
  CHECK (result_seal_no IS NULL OR result_type = 'ContainerVisit');

-- ISO 6346: an owner code of three letters, the equipment category U, J or Z, six digits and the check digit. A letter
-- stands for a number from 10 to 38 that leaves out the multiples of 11; each of the first ten characters is weighted
-- by a power of two, from 1 for the first to 512 for the tenth; the check digit is the sum modulo 11, and 0 where
-- that is 10.
CREATE FUNCTION cargo_sea.iso6346(num text) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $fn$
DECLARE
  total int := 0;
  v int;
BEGIN
  IF num IS NULL OR num !~ '^[A-Z]{3}[UJZ][0-9]{7}$' THEN
    RETURN false;
  END IF;
  FOR i IN 1..10 LOOP
    IF i <= 4 THEN
      v := ascii(substr(num, i, 1)) - 55;  -- A is 10
      v := v + (v - 1) / 10;               -- leaves out 11, 22 and 33
    ELSE
      v := ascii(substr(num, i, 1)) - 48;
    END IF;
    total := total + v * (1 << (i - 1));
  END LOOP;
  RETURN total % 11 % 10 = ascii(substr(num, 11, 1)) - 48;
END $fn$;

CREATE TABLE cargo_sea.container_visit (
 tenant_id text NOT NULL,
 visit_id text NOT NULL CHECK (length(btrim(visit_id)) BETWEEN 1 AND 128),
 movement_id text NOT NULL,
 consolidation_id text NOT NULL,
 load_unit_no integer NOT NULL CHECK (load_unit_no > 0), -- PHY's planned load unit of the consolidation
 container_number text NOT NULL,
 size_type text NOT NULL CHECK (size_type ~ '^[0-9A-Z]{2}[A-Z][0-9A-Z]$'),
 tare_kg bigint,
 max_gross_kg bigint,
 plate_source text,
 status text NOT NULL CHECK (status IN ('ASSIGNED','STUFFED','RELEASED')),
 execution_id text, -- PHY's stuffing of the unit, once STUFFED
 difference_reason text, -- why it was taken as stuffed although PHY's seal or size-type differs from the container's
 stuffed_by text,
 stuffed_at timestamptz,
 release_reason text,
 released_by text,
 released_at timestamptz,
 assigned_by text NOT NULL CHECK (length(btrim(assigned_by)) > 0),
 assigned_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 written_xact xid8 NOT NULL DEFAULT pg_current_xact_id(), -- the transaction that last wrote the visit, set by trigger
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, visit_id),
 FOREIGN KEY (tenant_id, movement_id) REFERENCES cargo_sea.ocean_movement (tenant_id, movement_id),
 CONSTRAINT container_visit_number CHECK (cargo_sea.iso6346(container_number)),
 -- The container's own tare and maximum gross, from its CSC plate or as the carrier states them, or none of the three.
 CONSTRAINT container_visit_plate CHECK (num_nulls(tare_kg, max_gross_kg, plate_source) = 3
   OR (num_nulls(tare_kg, max_gross_kg, plate_source) = 0 AND tare_kg > 0 AND max_gross_kg > tare_kg AND plate_source IN ('CSC_PLATE','CARRIER'))),
 CONSTRAINT container_visit_stuffing CHECK ((status = 'STUFFED') = (execution_id IS NOT NULL) AND (status = 'STUFFED') = (stuffed_at IS NOT NULL)
   AND (stuffed_by IS NULL) = (stuffed_at IS NULL) AND (execution_id IS NULL OR length(btrim(execution_id)) BETWEEN 1 AND 128)
   AND (stuffed_by IS NULL OR length(btrim(stuffed_by)) > 0) AND (stuffed_at IS NULL OR stuffed_at >= assigned_at)
   AND (difference_reason IS NULL OR (status = 'STUFFED' AND length(btrim(difference_reason)) > 0 AND length(difference_reason) <= 500))),
 CONSTRAINT container_visit_release CHECK ((status = 'RELEASED') = (released_at IS NOT NULL) AND (released_by IS NULL) = (released_at IS NULL)
   AND (release_reason IS NULL) = (released_at IS NULL) AND (released_by IS NULL OR length(btrim(released_by)) > 0)
   AND (release_reason IS NULL OR (length(btrim(release_reason)) > 0 AND length(release_reason) <= 500))
   AND (released_at IS NULL OR released_at >= assigned_at))
);
-- A container number has one live visit in the tenant, and a load unit of a movement one live container.
CREATE UNIQUE INDEX container_visit_live_number ON cargo_sea.container_visit (tenant_id, container_number) WHERE status <> 'RELEASED';
CREATE UNIQUE INDEX container_visit_live_unit ON cargo_sea.container_visit (tenant_id, movement_id, load_unit_no) WHERE status <> 'RELEASED';
CREATE INDEX container_visit_of_movement ON cargo_sea.container_visit (tenant_id, movement_id, assigned_at, visit_id);

-- Seals as they were applied: a further one replaces the current one, which stays as history.
CREATE TABLE cargo_sea.container_seal (
 tenant_id text NOT NULL,
 visit_id text NOT NULL,
 seal_no integer NOT NULL CHECK (seal_no > 0),
 number text NOT NULL CHECK (number ~ '^[0-9A-Z]+([ /-][0-9A-Z]+)*$' AND length(number) <= 35),
 party text NOT NULL CHECK (party IN ('CARRIER','SHIPPER','CUSTOMS')),
 applied_at timestamptz NOT NULL,
 source text NOT NULL CHECK (length(btrim(source)) > 0 AND length(source) <= 500),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, visit_id, seal_no),
 FOREIGN KEY (tenant_id, visit_id) REFERENCES cargo_sea.container_visit (tenant_id, visit_id),
 CONSTRAINT container_seal_applied CHECK (applied_at <= recorded_at)
);
CREATE TRIGGER container_seal_immutable BEFORE UPDATE OR DELETE ON cargo_sea.container_seal
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.reject_row_change();

-- A visit starts ASSIGNED at revision 1. It leaves ASSIGNED once, for STUFFED or RELEASED, writing only what that
-- step records; otherwise only its revision moves (a seal was recorded), by one each time, and a released visit
-- never changes. Nothing is deleted. Every write stamps the transaction, which the seals' check at commit reads.
CREATE FUNCTION cargo_sea.check_visit_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  step text[] := ARRAY['revision','updated_at','written_xact'];
  stuffing text[] := ARRAY['status','execution_id','difference_reason','stuffed_by','stuffed_at','revision','updated_at','written_xact'];
  release text[] := ARRAY['status','release_reason','released_by','released_at','revision','updated_at','written_xact'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_sea: container visit %/% is never deleted', OLD.tenant_id, OLD.visit_id USING ERRCODE = 'restrict_violation';
  END IF;
  NEW.written_xact := pg_current_xact_id();
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'ASSIGNED' OR NEW.revision <> 1 THEN
      RAISE EXCEPTION 'cargo_sea: container visit %/% starts ASSIGNED at revision 1', NEW.tenant_id, NEW.visit_id
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.revision = OLD.revision + 1 AND (
       (OLD.status = 'ASSIGNED' AND NEW.status = 'STUFFED' AND to_jsonb(OLD) - stuffing = to_jsonb(NEW) - stuffing)
    OR (OLD.status = 'ASSIGNED' AND NEW.status = 'RELEASED' AND to_jsonb(OLD) - release = to_jsonb(NEW) - release)
    OR (OLD.status <> 'RELEASED' AND NEW.status = OLD.status AND to_jsonb(OLD) - step = to_jsonb(NEW) - step)) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'cargo_sea: container visit %/% leaves ASSIGNED once, for STUFFED or RELEASED; otherwise only its revision moves, by one, and never once released',
    OLD.tenant_id, OLD.visit_id USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER container_visit_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_sea.container_visit
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_visit_change();

-- What must hold of a visit at commit, as the rule broken or NULL. The movement is read under a share lock, which a
-- cancellation and the movement's house commands wait for: a container and a cancellation cannot pass each other.
CREATE FUNCTION cargo_sea.visit_inconsistency(v cargo_sea.container_visit) RETURNS text LANGUAGE plpgsql AS $fn$
DECLARE
  m cargo_sea.ocean_movement;
  n int;
  top int;
  early boolean;
BEGIN
  SELECT * INTO m FROM cargo_sea.ocean_movement WHERE tenant_id = v.tenant_id AND movement_id = v.movement_id FOR SHARE;
  IF m.kind IS DISTINCT FROM 'SEA_LCL_OWN' OR m.consolidation_id IS DISTINCT FROM v.consolidation_id THEN
    RETURN 'a container is assigned on an own-container movement, for its consolidation';
  END IF;
  IF v.status <> 'RELEASED' AND m.state <> 'OPEN' THEN
    RETURN 'a movement that is not open holds no container';
  END IF;
  SELECT count(*), coalesce(max(s.seal_no), 0), coalesce(bool_or(s.recorded_at < v.assigned_at), false) INTO n, top, early
    FROM cargo_sea.container_seal s WHERE s.tenant_id = v.tenant_id AND s.visit_id = v.visit_id;
  IF top <> n THEN
    RETURN 'seals are numbered without gaps';
  END IF;
  IF early THEN
    RETURN 'a seal is recorded not before the container was assigned';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_sea.container_seal a JOIN cargo_sea.container_seal b
       ON b.tenant_id = a.tenant_id AND b.visit_id = a.visit_id AND b.seal_no = a.seal_no + 1
       WHERE a.tenant_id = v.tenant_id AND a.visit_id = v.visit_id AND (b.number = a.number OR b.recorded_at < a.recorded_at)) THEN
    RETURN 'a further seal has another number and is recorded not before the one it replaces';
  END IF;
  IF v.status = 'STUFFED' AND n = 0 THEN
    RETURN 'a stuffed container has a seal';
  END IF;
  RETURN NULL;
END $fn$;

-- At commit a visit is consistent; and the step to STUFFED finds, among the live houses of its movement, a sealed
-- stuffing of its load unit with the execution it keeps, sealed not after.
CREATE FUNCTION cargo_sea.check_visit_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_sea.container_visit;
  broken text;
BEGIN
  SELECT * INTO v FROM cargo_sea.container_visit WHERE tenant_id = NEW.tenant_id AND visit_id = NEW.visit_id;
  broken := cargo_sea.visit_inconsistency(v);
  IF broken IS NULL AND TG_OP = 'UPDATE' AND OLD.status = 'ASSIGNED' AND NEW.status = 'STUFFED' AND NOT EXISTS (
       SELECT 1 FROM cargo_sea.house_bill e JOIN cargo_sea.house_bill_stuffing s
         ON s.tenant_id = e.tenant_id AND s.movement_id = e.movement_id AND s.entry_no = e.entry_no
       WHERE e.tenant_id = NEW.tenant_id AND e.movement_id = NEW.movement_id AND e.ended_at IS NULL
         AND s.execution_id = NEW.execution_id AND s.load_unit_no = NEW.load_unit_no AND s.sealed_at <= NEW.stuffed_at) THEN
    broken := 'a container is stuffed once a live house of its movement carries a sealed stuffing of its load unit';
  END IF;
  IF broken IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_sea: container visit %/%: %', NEW.tenant_id, NEW.visit_id, broken
      USING ERRCODE = 'check_violation', CONSTRAINT = 'container_visit_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER container_visit_consistent AFTER INSERT OR UPDATE ON cargo_sea.container_visit
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_visit_at_commit();

-- At commit a seal is of a visit that is not released and that the same transaction wrote (each seal is a step of the
-- visit's revision), and the visit is consistent with it.
CREATE FUNCTION cargo_sea.check_seal_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_sea.container_visit;
  broken text;
BEGIN
  SELECT * INTO v FROM cargo_sea.container_visit WHERE tenant_id = NEW.tenant_id AND visit_id = NEW.visit_id FOR SHARE;
  IF v.status = 'RELEASED' THEN
    broken := 'a released container takes no seal';
  ELSIF v.written_xact <> pg_current_xact_id() THEN
    broken := 'a seal is written with a step of its visit''s revision';
  ELSE
    broken := cargo_sea.visit_inconsistency(v);
  END IF;
  IF broken IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_sea: seal %/%/%: %', NEW.tenant_id, NEW.visit_id, NEW.seal_no, broken
      USING ERRCODE = 'check_violation', CONSTRAINT = 'container_seal_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER container_seal_consistent AFTER INSERT ON cargo_sea.container_seal
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_seal_at_commit();

-- A movement is cancelled without containers: every visit released first. The row is locked before this runs, so a
-- visit whose check at commit holds its share lock is seen.
CREATE FUNCTION cargo_sea.check_movement_holds_no_container() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.state = 'CANCELLED' AND OLD.state <> 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_sea.container_visit v
       WHERE v.tenant_id = NEW.tenant_id AND v.movement_id = NEW.movement_id AND v.status <> 'RELEASED') THEN
    RAISE EXCEPTION 'cargo_sea: movement %/% holds a container', NEW.tenant_id, NEW.movement_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_movement_holds_no_container';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER ocean_movement_holds_no_container BEFORE UPDATE ON cargo_sea.ocean_movement
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_movement_holds_no_container();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['container_visit','container_seal'] LOOP
    EXECUTE format('ALTER TABLE cargo_sea.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_sea.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_sea.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
