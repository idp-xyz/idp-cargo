-- IDP-Cargo SEA V0.2 (sea freight part 1e, facts after finalization) / PostgreSQL 16.
-- Run only as the migrator role, never at service start.
-- A FINAL movement may still hold a live container. A gate-in, a loading, another container fact and the carrier's
-- acceptance of a VGM are written while it stays FINAL. A new seal, a new VGM assertion or the carrier's rejection
-- changes the published facts, so it is written only once the same transaction has returned the movement to OPEN.
-- A cancelled movement still holds no container. 000001 through 000005 are unchanged.

-- A live container sits on an OPEN or FINAL movement. A cancelled movement holds none. The message stays the one
-- the cancelled-movement rule is known by.
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
  gated boolean;
  loaded boolean;
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
         coalesce(bool_or(e.kind = 'GATE_IN_TERMINAL'), false), coalesce(bool_or(e.kind = 'LOADED_ON_BOARD'), false)
    INTO events, event_top, gated, loaded
    FROM cargo_sea.container_event e WHERE e.tenant_id = v.tenant_id AND e.visit_id = v.visit_id;
  IF event_top <> events THEN
    RETURN 'container events are numbered without gaps';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_sea.container_event e WHERE e.tenant_id = v.tenant_id AND e.visit_id = v.visit_id AND e.recorded_at < v.assigned_at) THEN
    RETURN 'a container event is recorded not before the container was assigned';
  END IF;
  IF (v.status IN ('GATED_IN','LOADED')) IS DISTINCT FROM gated THEN
    RETURN 'a container is gated in exactly when a gate-in is recorded, and stays so once loaded';
  END IF;
  IF (v.status = 'LOADED') IS DISTINCT FROM loaded THEN
    RETURN 'a container is loaded exactly when it is recorded loaded on board';
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

-- A seal changes the published facts, so at commit the movement is OPEN: the command reopened it in this transaction.
CREATE OR REPLACE FUNCTION cargo_sea.check_seal_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
  IF broken IS NULL AND (SELECT state FROM cargo_sea.ocean_movement m
       WHERE m.tenant_id = v.tenant_id AND m.movement_id = v.movement_id) IS DISTINCT FROM 'OPEN' THEN
    broken := 'a seal is recorded while the movement is open';
  END IF;
  IF broken IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_sea: seal %/%/%: %', NEW.tenant_id, NEW.visit_id, NEW.seal_no, broken
      USING ERRCODE = 'check_violation', CONSTRAINT = 'container_seal_consistent';
  END IF;
  RETURN NULL;
END $fn$;

-- A new assertion is recorded only while the movement is OPEN, because it changes the published facts. The carrier's
-- acceptance may be written while the movement is OPEN or FINAL. A rejection is written only while the movement is
-- OPEN: it withdraws the VGM the published facts rely on. The cut-off is still checked only on insert.
CREATE OR REPLACE FUNCTION cargo_sea.check_vgm_assertion_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_sea.container_visit;
  cut timestamptz;
  st text;
BEGIN
  SELECT * INTO v FROM cargo_sea.container_visit WHERE tenant_id = NEW.tenant_id AND visit_id = NEW.visit_id FOR SHARE;
  IF v.status IS DISTINCT FROM 'VGM_SUBMITTED' OR (TG_OP = 'INSERT' AND v.written_xact IS DISTINCT FROM pg_current_xact_id()) THEN
    RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% is written with a step of a submitted visit', NEW.tenant_id, NEW.visit_id, NEW.version
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
  END IF;
  IF TG_OP = 'UPDATE' AND v.written_xact IS DISTINCT FROM pg_current_xact_id() THEN
    RAISE EXCEPTION 'cargo_sea: the carrier''s answer to VGM assertion %/%/% is written with a step of the visit', NEW.tenant_id, NEW.visit_id, NEW.version
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
  END IF;
  SELECT m.state INTO st FROM cargo_sea.ocean_movement m WHERE m.tenant_id = v.tenant_id AND m.movement_id = v.movement_id;
  IF TG_OP = 'INSERT' AND st IS DISTINCT FROM 'OPEN' THEN
    RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% is recorded while the movement is open', NEW.tenant_id, NEW.visit_id, NEW.version
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
  END IF;
  IF TG_OP = 'UPDATE' AND NEW.response = 'REJECTED' AND st IS DISTINCT FROM 'OPEN' THEN
    RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% is rejected while the movement is open', NEW.tenant_id, NEW.visit_id, NEW.version
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
  END IF;
  IF TG_OP = 'UPDATE' AND st IS DISTINCT FROM 'OPEN' AND st IS DISTINCT FROM 'FINAL' THEN
    RAISE EXCEPTION 'cargo_sea: the carrier''s answer to VGM assertion %/%/% is recorded while the movement is open or final', NEW.tenant_id, NEW.visit_id, NEW.version
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
  END IF;
  IF TG_OP = 'INSERT' THEN
    SELECT c.cut_off_at INTO cut FROM cargo_sea.voyage_cut_off c
      JOIN cargo_sea.booking_link l ON l.tenant_id = c.tenant_id AND l.movement_id = c.movement_id AND l.link_no = c.link_no
      WHERE c.tenant_id = v.tenant_id AND c.movement_id = v.movement_id AND c.leg_no = 1 AND c.kind = 'VGM'
        AND l.link_no = (SELECT max(link_no) FROM cargo_sea.booking_link WHERE tenant_id = v.tenant_id AND movement_id = v.movement_id);
    -- A missing cut-off makes "submitted after it" false, so a reason is refused; after it, a reason is required.
    IF (cut IS NOT NULL AND NEW.submitted_at > cut) IS DISTINCT FROM (NEW.late_reason IS NOT NULL) THEN
      RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% has a late reason exactly when it is submitted after the VGM cut-off', NEW.tenant_id, NEW.visit_id, NEW.version
        USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
    END IF;
  END IF;
  RETURN NULL;
END $fn$;
