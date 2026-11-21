-- IDP-Cargo SEA V2.89 (a container taken back out of the terminal) / PostgreSQL 16.
-- Run only as the migrator role, never at service start. 000001 through 000010 are unchanged.
-- A new container event kind, GATE_OUT_TERMINAL: the full container taken back out of the terminal to the forwarder's
-- warehouse. A gated-in visit then returns to VGM_SUBMITTED, so that a VGM can be declared again and the container can
-- gate in again; a loaded container is not taken back. A visit is GATED_IN or LOADED exactly when its latest gate-in
-- (by event number) comes after its latest gate-out, LOADED exactly when a loading is recorded, and no gate-out comes
-- after a loading. A gate-out follows a gate-in of the same visit with no gate-out between them (by event number).
-- No row written before this migration has the kind (the kind check of 000004 refused it), so the new rule meets no
-- existing row.
-- Objects replaced, each with every rule of its latest earlier definition:
--   cargo_sea.container_event kind check  000004 (gains GATE_OUT_TERMINAL)
--   cargo_sea.check_visit_change()        000010 (gains GATED_IN to VGM_SUBMITTED, only the status changes)
--   cargo_sea.visit_inconsistency()       000006 (000007 to 000010 left it; its gate and loading rules change)
-- check_container_event_at_commit() (000008) is not replaced: it takes the visit's status under FOR SHARE and asks
-- visit_inconsistency, so it applies the new rules to the new kind as it is; its loading rule is for LOADED_ON_BOARD.
-- Locks: DROP CONSTRAINT and ADD CONSTRAINT take ACCESS EXCLUSIVE on cargo_sea.container_event for their own
-- duration; no table is rewritten, the new CHECK scans the table once, and no row is written. CREATE OR REPLACE
-- FUNCTION takes no table lock. No column is added, so the grants step does not change.
-- A conflict between SEA's records and a carrier's loading report at a reseal is released by the operator with a
-- reason (the owner's decision of 2026-11-21): cargo_sea.carrier_conflict_release, one immutable row per release,
-- numbered from 1 per visit without gaps, of a LOADED_ON_BOARD fact of the visit's movement, with PHY's seal (as the
-- houses carry it) and its sealed-at, the reason (1 to 500 characters), who, when and when entered, one release at most
-- for a report against a seal and its sealed-at; written with a step
-- of its visit's revision, on a visit that is stuffed or later, while its movement is OPEN at commit. Row level
-- security as the other tables; the application role's grants are the schema-wide SELECT and INSERT of the grants step
-- (.github/workflows/org-pg16-contract.yml), which covers a new table, so that file does not change. The command
-- receipt gains result_release_no (a column of a table the application role inserts into whole).
-- What only the domain keeps: that a gate-out is not dated before the gate-in it follows (the database accepts it; the
-- model refuses it in RecordContainerEvent), that a command's business time is not before the visit's last change, and
-- that a release is of a conflict the container's pending item reports (the database checks that the report is a
-- loading of the movement, not that it was dated before PHY's seal, nor that the seal is PHY's, nor that the
-- container's own events leave the conflict open).

ALTER TABLE cargo_sea.container_event DROP CONSTRAINT container_event_kind_check;
ALTER TABLE cargo_sea.container_event ADD CONSTRAINT container_event_kind_check CHECK (kind IN (
  'EMPTY_RELEASED','EMPTY_PICKED_UP','ARRIVED_AT_CFS','GATE_IN_TERMINAL','GATE_OUT_TERMINAL','LOADED_ON_BOARD','ROLLED'));

CREATE OR REPLACE FUNCTION cargo_sea.check_visit_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  step text[] := ARRAY['revision','updated_at','written_xact'];
  stuffing text[] := ARRAY['status','execution_id','difference_reason','stuffed_by','stuffed_at','stuffed_phy_seal','stuffed_phy_sealed_at','revision','updated_at','written_xact'];
  release text[] := ARRAY['status','release_reason','released_by','released_at','revision','updated_at','written_xact'];
  move text[] := ARRAY['status','revision','updated_at','written_xact'];
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
    OR (OLD.status = 'STUFFED' AND NEW.status = 'VGM_SUBMITTED' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status = 'VGM_SUBMITTED' AND NEW.status = 'GATED_IN' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status = 'GATED_IN' AND NEW.status = 'LOADED' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status = 'GATED_IN' AND NEW.status = 'VGM_SUBMITTED' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status <> 'RELEASED' AND NEW.status = OLD.status AND to_jsonb(OLD) - step = to_jsonb(NEW) - step)) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'cargo_sea: container visit %/% leaves ASSIGNED once, for STUFFED or RELEASED; STUFFED only for VGM_SUBMITTED, that only for GATED_IN, GATED_IN only for LOADED or back to VGM_SUBMITTED; otherwise only its revision moves, by one, and never once released',
    OLD.tenant_id, OLD.visit_id USING ERRCODE = 'restrict_violation';
END $fn$;

CREATE OR REPLACE FUNCTION cargo_sea.visit_inconsistency(v cargo_sea.container_visit) RETURNS text LANGUAGE plpgsql AS $fn$
DECLARE
  m cargo_sea.ocean_movement;
  n int;
  top int;
  early boolean;
  versions int;
  version_top int;
  signed int;
  events int;
  event_top int;
  last_in int;
  last_out int;
  last_load int;
BEGIN
  SELECT * INTO m FROM cargo_sea.ocean_movement WHERE tenant_id = v.tenant_id AND movement_id = v.movement_id FOR SHARE;
  IF m.kind IS DISTINCT FROM 'SEA_LCL_OWN' OR m.consolidation_id IS DISTINCT FROM v.consolidation_id THEN
    RETURN 'a container is assigned on an own-container movement, for its consolidation';
  END IF;
  IF v.status <> 'RELEASED' AND m.state = 'CANCELLED' THEN
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
  IF v.status IN ('STUFFED','VGM_SUBMITTED','GATED_IN','LOADED') AND n = 0 THEN
    RETURN 'a stuffed container has a seal';
  END IF;
  SELECT count(*), coalesce(max(e.event_no), 0),
         coalesce(max(e.event_no) FILTER (WHERE e.kind = 'GATE_IN_TERMINAL'), 0),
         coalesce(max(e.event_no) FILTER (WHERE e.kind = 'GATE_OUT_TERMINAL'), 0),
         coalesce(max(e.event_no) FILTER (WHERE e.kind = 'LOADED_ON_BOARD'), 0)
    INTO events, event_top, last_in, last_out, last_load
    FROM cargo_sea.container_event e WHERE e.tenant_id = v.tenant_id AND e.visit_id = v.visit_id;
  IF event_top <> events THEN
    RETURN 'container events are numbered without gaps';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_sea.container_event e WHERE e.tenant_id = v.tenant_id AND e.visit_id = v.visit_id AND e.recorded_at < v.assigned_at) THEN
    RETURN 'a container event is recorded not before the container was assigned';
  END IF;
  IF (v.status IN ('GATED_IN','LOADED')) IS DISTINCT FROM (last_in > last_out) THEN
    RETURN 'a container is gated in or loaded exactly when its latest gate-in comes after its latest gate-out';
  END IF;
  IF (v.status = 'LOADED') IS DISTINCT FROM (last_load > 0) THEN
    RETURN 'a container is loaded exactly when it is recorded loaded on board';
  END IF;
  IF last_load > 0 AND last_out > last_load THEN
    RETURN 'a container is taken back out of the terminal only while it is not loaded: no gate-out comes after a loading';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_sea.container_event o WHERE o.tenant_id = v.tenant_id AND o.visit_id = v.visit_id
       AND o.kind = 'GATE_OUT_TERMINAL'
       AND (SELECT p.kind FROM cargo_sea.container_event p WHERE p.tenant_id = o.tenant_id AND p.visit_id = o.visit_id
              AND p.kind IN ('GATE_IN_TERMINAL','GATE_OUT_TERMINAL') AND p.event_no < o.event_no
            ORDER BY p.event_no DESC LIMIT 1) IS DISTINCT FROM 'GATE_IN_TERMINAL') THEN
    RETURN 'a gate-out follows a gate-in of the same container with no gate-out between them';
  END IF;
  IF v.status IN ('VGM_SUBMITTED','GATED_IN','LOADED') THEN
    SELECT count(*), coalesce(max(a.version), 0) INTO versions, version_top FROM cargo_sea.vgm_assertion a
      WHERE a.tenant_id = v.tenant_id AND a.visit_id = v.visit_id;
    SELECT cargo_sea.vgm_signatory_count(v.visit_id) INTO signed;
    IF versions = 0 OR version_top <> versions OR signed <> versions THEN
      RETURN 'a submitted container has its VGM assertions, numbered without gaps, each with its signatory';
    END IF;
    IF v.tare_kg IS NOT NULL AND EXISTS (SELECT 1 FROM cargo_sea.vgm_assertion a
         WHERE a.tenant_id = v.tenant_id AND a.visit_id = v.visit_id AND a.mass_grams < v.tare_kg * 1000) THEN
      RETURN 'the verified gross mass is not below the tare';
    END IF;
  END IF;
  RETURN NULL;
END $fn$;

-- The release of a conflict with a carrier's loading report. A command that writes one names it in its receipt.
ALTER TABLE cargo_sea.command_receipt ADD COLUMN result_release_no integer CHECK (result_release_no > 0);
ALTER TABLE cargo_sea.command_receipt ADD CONSTRAINT command_receipt_release_of_visit
  CHECK (result_release_no IS NULL OR result_type = 'ContainerVisit');

CREATE TABLE cargo_sea.carrier_conflict_release (
 tenant_id text NOT NULL,
 visit_id text NOT NULL,
 release_no integer NOT NULL CHECK (release_no > 0),
 fact_no integer NOT NULL CHECK (fact_no > 0),
 phy_seal text NOT NULL CHECK (length(btrim(phy_seal)) > 0 AND length(phy_seal) <= 500),
 phy_sealed_at timestamptz NOT NULL,
 reason text NOT NULL CHECK (length(btrim(reason)) > 0 AND length(reason) <= 500),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, visit_id, release_no),
 FOREIGN KEY (tenant_id, visit_id) REFERENCES cargo_sea.container_visit (tenant_id, visit_id),
 CONSTRAINT carrier_conflict_release_once UNIQUE (tenant_id, visit_id, fact_no, phy_seal, phy_sealed_at)
);
CREATE TRIGGER carrier_conflict_release_immutable BEFORE UPDATE OR DELETE ON cargo_sea.carrier_conflict_release
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.reject_row_change();

-- At commit: the visit is stuffed or later, not released, and this transaction wrote it (a step of its revision); it
-- is consistent; its movement is OPEN (a release does not change the published facts, but only an open movement is
-- finalized afterwards); the fact is a loading of that movement; the releases are numbered from 1 without gaps. The
-- visit is read under its share lock before its state, and visit_inconsistency holds the movement's share lock.
CREATE FUNCTION cargo_sea.check_carrier_conflict_release_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_sea.container_visit;
  broken text;
  n int;
  top int;
BEGIN
  SELECT * INTO v FROM cargo_sea.container_visit WHERE tenant_id = NEW.tenant_id AND visit_id = NEW.visit_id FOR SHARE;
  IF v.status IN ('ASSIGNED','RELEASED') THEN
    broken := 'a conflict is released on a stuffed container';
  ELSIF v.written_xact IS DISTINCT FROM pg_current_xact_id() THEN
    broken := 'a release is written with a step of its visit''s revision';
  ELSE
    broken := cargo_sea.visit_inconsistency(v);
  END IF;
  IF broken IS NULL AND (SELECT m.state FROM cargo_sea.ocean_movement m
       WHERE m.tenant_id = v.tenant_id AND m.movement_id = v.movement_id) IS DISTINCT FROM 'OPEN' THEN
    broken := 'a conflict is released only while the movement is open';
  END IF;
  IF broken IS NULL AND NOT EXISTS (SELECT 1 FROM cargo_sea.ocean_carrier_fact f
       WHERE f.tenant_id = v.tenant_id AND f.movement_id = v.movement_id AND f.fact_no = NEW.fact_no AND f.kind = 'LOADED_ON_BOARD') THEN
    broken := 'a release is of a loading the carrier reported on the visit''s movement';
  END IF;
  IF broken IS NULL THEN
    SELECT count(*), coalesce(max(r.release_no), 0) INTO n, top FROM cargo_sea.carrier_conflict_release r
      WHERE r.tenant_id = v.tenant_id AND r.visit_id = v.visit_id;
    IF top <> n THEN
      broken := 'releases are numbered without gaps';
    END IF;
  END IF;
  IF broken IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_sea: release %/%/%: %', NEW.tenant_id, NEW.visit_id, NEW.release_no, broken
      USING ERRCODE = 'check_violation', CONSTRAINT = 'carrier_conflict_release_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER carrier_conflict_release_consistent AFTER INSERT ON cargo_sea.carrier_conflict_release
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_carrier_conflict_release_at_commit();

ALTER TABLE cargo_sea.carrier_conflict_release ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_sea.carrier_conflict_release FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_sea.carrier_conflict_release
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
